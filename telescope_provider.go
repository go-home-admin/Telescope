package telescope

import (
	"bufio"
	"bytes"
	"encoding/json"
	"github.com/gin-gonic/gin"
	"github.com/go-home-admin/home/app"
	uuid "github.com/satori/go.uuid"
	"github.com/sirupsen/logrus"
	"gorm.io/gorm"
	"io"
	"net"
	"net/http"
	"net/url"
	"os"
	"runtime"
	"runtime/debug"
	"strconv"
	"strings"
	"sync"
	"time"
)

// Providers @Bean
type Providers struct {
	Mysql  *gorm.DB `inject:"mysql, @config(telescope.connect, default)"`
	isOpen bool

	init bool
}

func (t *Providers) Init() {
	if app.IsDebug() {
		t.isOpen = true
	}
}

func (t *Providers) Boot() {
	if t.isOpen && !t.init {
		t.init = true
		t.SetLog()
	}
}

func (t *Providers) IsEnable() bool {
	return t.isOpen
}

// SetLog 打开望远镜时候, 设置log
func (t *Providers) SetLog() {
	hook := NewtelescopeHook()
	hook.mysql = t.Mysql
	hostname, _ := os.Hostname()
	hook.hostname = "home-server@" + hostname
	logrus.AddHook(hook)
}

// @Bean
type telescopeHook struct {
	mysql     *gorm.DB
	CidToUUID sync.Map
	hostname  string
}

func (t *telescopeHook) Levels() []logrus.Level {
	return logrus.AllLevels
}

func (t *telescopeHook) Fire(entry *logrus.Entry) error {
	m, ok := entry.Data["type"]
	if !ok {
		m = EntryTypeLOG
	}
	mtype := m.(string)
	var content map[string]interface{}
	switch mtype {
	case EntryTypeQUERY:
		content, ok = t.EntryTypeQUERY(entry)
		if !ok {
			return nil
		}
	case EntryTypeREQUEST:
		content, ok = t.EntryTypeREQUEST(entry)
		if !ok {
			return nil
		}
	case EntryTypeREDIS:
		content = t.EntryTypeREDIS(entry)
	case EntryTypeJOB:
		content = t.EntryTypeJob(entry)
	default:
		content = t.EntryTypeLOG(entry)
	}
	contentStr, err := json.Marshal(content)
	if err != nil {
		contentStr = []byte("无法格式化log to json")
	}
	id := uuid.NewV4().String()
	data := map[string]interface{}{
		"uuid":                    id,
		"batch_id":                t.TelescopeUUID(),
		"family_hash":             nil,
		"should_display_on_index": 1,
		"type":                    mtype,
		"content":                 string(contentStr),
		"created_at":              time.Now().Format("2006-01-02 15:04:05"),
	}

	res := t.mysql.Table("telescope_entries").Create(data)
	if res.Error == nil {
		t.CreateTag(id, mtype, content, entry)
	}
	return nil
}

func (t *telescopeHook) EntryTypeLOG(entry *logrus.Entry) map[string]interface{} {
	if entry.Level <= logrus.Level(logrus.ErrorLevel) {
		entry.Data["debug"] = string(debug.Stack())
	}
	return map[string]interface{}{
		"level":    entry.Level,
		"message":  entry.Message,
		"context":  entry.Data,
		"hostname": t.hostname,
	}
}

func (t *telescopeHook) EntryTypeQUERY(entry *logrus.Entry) (map[string]interface{}, bool) {
	if strings.Index(entry.Message, "telescope_") != -1 {
		return nil, false
	}

	var file, line string
	if entry.HasCaller() {
		file = entry.Caller.File
		line = strconv.Itoa(entry.Caller.Line)
	}
	return map[string]interface{}{
		"connection": "Mysql",
		"bindings":   "",
		"sql":        entry.Message,
		"time":       "0",
		"slow":       false,
		"file":       file,
		"line":       line,
		"hash":       "",
		"hostname":   t.hostname,
	}, true
}

func (t *telescopeHook) EntryTypeREQUEST(entry *logrus.Entry) (map[string]interface{}, bool) {
	var ctx interface{}
	var res interface{}
	ctx = entry.Context
	ginCtx := ctx.(*gin.Context)
	res = ginCtx.Writer
	telescopeResp := res.(*TelescopeResponseWriter)

	var response interface{}
	responseJSON := map[string]interface{}{}
	err := json.Unmarshal(telescopeResp.Body.Bytes(), &responseJSON)
	if err != nil || len(responseJSON) == 0 {
		response = telescopeResp.Body.String()
	} else {
		response = responseJSON
	}

	// 原始请求数据
	payload := make(map[string]interface{})
	if ginCtx.Request.PostForm == nil {
		raw, ok := ginCtx.Get("raw")
		if ok {
			switch raw.(type) {
			case string:
				data := raw.(string)
				_ = json.Unmarshal([]byte(data), &payload)
			case []byte:
				data := raw.([]byte)
				_ = json.Unmarshal(data, &payload)
			}
		}
	} else {
		for k, v := range ginCtx.Request.PostForm {
			payload[k] = v[0]
		}
	}
	var duration time.Duration
	start, ok := ginCtx.Get("start")
	if ok {
		duration = time.Now().Sub(start.(time.Time))
	} else {
		duration = 0
	}

	return map[string]interface{}{
		"ip_address": ginCtx.ClientIP(),
		"uri":        entry.Message,
		"method":     ginCtx.Request.Method,
		//"controller_action": "",
		//"middleware":        []string{},
		"headers": ginCtx.Request.Header,
		"payload": payload,
		//"session":           nil,
		"response_status": ginCtx.Writer.Status(),
		"response":        response,
		"duration":        duration.Milliseconds(),
		"memory":          ginCtx.Writer.Size(),
		"hostname":        t.hostname,
	}, true
}

func (t *telescopeHook) EntryTypeREDIS(entry *logrus.Entry) map[string]interface{} {
	return map[string]interface{}{
		"connection": "cache",
		"command":    entry.Message,
		"time":       "0",
		"hostname":   t.hostname,
	}
}

func (t *telescopeHook) EntryTypeJob(entry *logrus.Entry) map[string]interface{} {
	ginCtx := entry.Context.(*gin.Context)
	data, _ := ginCtx.Get("telescope_data")
	res := data.(map[string]interface{})
	res["hostname"] = t.hostname
	return res
}

func (t *telescopeHook) CreateTag(uuid, mType string, content map[string]interface{}, entry *logrus.Entry) {
	var tag string
	switch mType {
	case "log":
		if _, ok := content["level"]; ok {
			tag = content["level"].(logrus.Level).String()
		}
	case "query":
		if _, ok := content["show"]; ok {
			if content["show"].(bool) {
				tag = "show"
			}
		}
	case "request":
		if _, ok := content["uri"]; ok {
			u, err := url.Parse(content["uri"].(string))
			if err == nil && u != nil {
				tag = u.Path
			}
		}
	case "job":
		if _, ok := content["status"]; ok {
			if content["status"].(string) == "failed" {
				tag = "failed"
			}
		}
	default:
		tag = ""
	}
	if tag != "" {
		t.mysql.Table("telescope_entries_tags").Create(map[string]interface{}{
			"entry_uuid": uuid,
			"tag":        tag,
		})
	}
	// WithFields(Fields{"type": "request", "tags": []string{"test"}})
	tags, ok := entry.Data["tags"]
	if ok {
		if v, ok := tags.([]string); ok {
			for _, tag := range v {
				t.mysql.Table("telescope_entries_tags").Create(map[string]interface{}{
					"entry_uuid": uuid,
					"tag":        tag,
				})
			}
		}
	}
}

func (t *telescopeHook) TelescopeUUID() string {
	cid := getGoId()
	v, ok := t.CidToUUID.Load(cid)

	if ok {
		return v.(string)
	}
	// 未开启情况就是使用，是无法关联的
	// 需要先使用 TelescopeStart()
	return time.Now().Format("2006-01-02 15:04:05")
}

func TelescopeStart() {
	cid := getGoId()
	t := NewtelescopeHook()
	t.CidToUUID.Store(cid, uuid.NewV4().String())
}

func TelescopeClose() {
	cid := getGoId()
	t := NewtelescopeHook()
	t.CidToUUID.Delete(cid)
}

// 获取跟踪ID, 严禁非开发模式使用
// github.com/bigwhite/experiments/blob/master/trace-function-call-chain/trace3/trace.go
func getGoId() uint64 {
	b := make([]byte, 64)
	b = b[:runtime.Stack(b, false)]
	b = bytes.TrimPrefix(b, []byte("goroutine "))
	b = b[:bytes.IndexByte(b, ' ')]
	n, _ := strconv.ParseUint(string(b), 10, 64)
	return n
}

type TelescopeResponseWriter struct {
	gin.ResponseWriter
	Body *bytes.Buffer
}

func (w TelescopeResponseWriter) Write(b []byte) (int, error) {
	w.Body.Write(b)
	return w.ResponseWriter.Write(b)
}

func (w TelescopeResponseWriter) WriteString(s string) (int, error) {
	w.Body.WriteString(s)
	return w.ResponseWriter.WriteString(s)
}

type ResponseWriter struct {
	http.ResponseWriter
	size   int
	status int
}

func (w *ResponseWriter) reset(writer http.ResponseWriter) {
	w.ResponseWriter = writer
	w.size = -1
	w.status = 200
}

func (w *ResponseWriter) WriteHeader(code int) {
	if code > 0 && w.status != code {
		w.status = code
	}
}

func (w *ResponseWriter) WriteHeaderNow() {
	if !w.Written() {
		w.size = 0
		w.ResponseWriter.WriteHeader(w.status)
	}
}

func (w *ResponseWriter) Write(data []byte) (n int, err error) {
	w.WriteHeaderNow()
	n, err = w.ResponseWriter.Write(data)
	w.size += n
	return
}

func (w *ResponseWriter) WriteString(s string) (n int, err error) {
	w.WriteHeaderNow()
	n, err = io.WriteString(w.ResponseWriter, s)
	w.size += n
	return
}

func (w *ResponseWriter) Status() int {
	return w.status
}

func (w *ResponseWriter) Size() int {
	return w.size
}

func (w *ResponseWriter) Written() bool {
	return w.size != -1
}

// Hijack implements the http.Hijacker interface.
func (w *ResponseWriter) Hijack() (net.Conn, *bufio.ReadWriter, error) {
	if w.size < 0 {
		w.size = 0
	}
	return w.ResponseWriter.(http.Hijacker).Hijack()
}

// CloseNotify implements the http.CloseNotifier interface.
func (w *ResponseWriter) CloseNotify() <-chan bool {
	return w.ResponseWriter.(http.CloseNotifier).CloseNotify()
}

// Flush implements the http.Flusher interface.
func (w *ResponseWriter) Flush() {
	w.WriteHeaderNow()
	w.ResponseWriter.(http.Flusher).Flush()
}

func (w *ResponseWriter) Pusher() (pusher http.Pusher) {
	if pusher, ok := w.ResponseWriter.(http.Pusher); ok {
		return pusher
	}
	return nil
}

var EntryTypeBATCH = "batch"
var EntryTypeCACHE = "cache"
var EntryTypeCOMMAND = "command"
var EntryTypeDUMP = "dump"
var EntryTypeEVENT = "event"
var EntryTypeEXCEPTION = "exception"
var EntryTypeJOB = "job"
var EntryTypeLOG = "log"
var EntryTypeMAIL = "mail"
var EntryTypeMODEL = "model"
var EntryTypeNOTIFICATION = "notification"
var EntryTypeQUERY = "query"
var EntryTypeREDIS = "redis"
var EntryTypeREQUEST = "request"
var EntryTypeSCHEDULED_TASK = "schedule"
var EntryTypeGATE = "gate"
var EntryTypeVIEW = "view"
var EntryTypeCLIENT_REQUEST = "home_request"
