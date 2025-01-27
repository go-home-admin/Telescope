package telescope

import (
	"bytes"
	"github.com/gin-gonic/gin"
	"github.com/go-home-admin/home/app"
	uuid "github.com/satori/go.uuid"
	"github.com/sirupsen/logrus"
	"gorm.io/gorm"
	"os"
	"runtime"
	"strconv"
	"sync"
	"time"
)

// Providers @Bean
type Providers struct {
	//TODO 默认配置default不生效，config不声明telescope.connect会报错
	Mysql  *gorm.DB `inject:"mysql, @config(telescope.connect, default)"`
	isOpen bool

	init bool
}

var Routes = make(map[string]Type)
var (
	errorRecord bool //为true时，即使非debug模式也会开启望远镜，但只记录出错日志
	hasError    bool //标记错误日志，以便记录request为错误请求
	isSkip      bool //当为SkipPathList里的路径时，跳过所有收集
)

// SkipPathList Requests in the list are not logged. Supports paths ending with a wildcard `*`. For example:
// "/test" means only "/test" is not logged.
// "/test*" means "/test" and all requests with "/test" as a prefix are not logged.
// "/test/*" means all requests with "/test" as a prefix are not logged, but "/test" is logged.
var SkipPathList []string

// SetDB 任意框架下使用， 需要手动设置DB
func (t *Providers) SetDB(db *gorm.DB) {
	t.Mysql = db
}

func (t *Providers) Init() {
	v := app.GetConfigAny("telescope.error_record")
	if b, ok := v.(*bool); ok && *b {
		errorRecord = true
	}
	if (app.IsDebug() || errorRecord) && !t.init {
		t.init = true
		t.Register()
	}
}

func (t *Providers) Register() {
	for _, i := range GetAllProvider() {
		if v, ok := i.(Type); ok {
			Routes[v.BindType()] = v
		}
	}

	t.SetLog()
}

// SetLog 打开望远镜时候, 设置log
func (t *Providers) SetLog() {
	hook := NewtelescopeHook()
	hook.mysql = t.Mysql
	hostname, _ := os.Hostname()
	hook.hostname = "home-server@" + hostname
	logrus.AddHook(hook)
}

// AddRoute 允许重载处理
func (t *Providers) AddRoute(v Type) {
	Routes[v.BindType()] = v
}

// @Bean
type telescopeHook struct {
	mysql       *gorm.DB
	CidToUUID   sync.Map
	hostname    string
	isOnlyRoute bool
}

func (t *telescopeHook) Init() {
	v := app.GetConfigAny("telescope.is_only_route")
	if b, ok := v.(*bool); ok && *b {
		t.isOnlyRoute = true
	}
}

func (t *telescopeHook) Levels() []logrus.Level {
	if !app.IsDebug() {
		return []logrus.Level{
			logrus.PanicLevel,
			logrus.FatalLevel,
			logrus.ErrorLevel,
		}
	}
	return logrus.AllLevels
}

func (t *telescopeHook) Fire(entry *logrus.Entry) error {
	if isSkip {
		//Path in PathSkipList
		return nil
	}
	m, ok := entry.Data["type"]
	if !ok {
		m = "log"
	}
	mType := m.(string)
	//忽略路由以外的请求
	if mType == "request" && t.isOnlyRoute {
		ctx := entry.Context.(*gin.Context)
		if ctx.FullPath() == "" {
			return nil
		}
	}
	route, ok := Routes[mType]
	if ok {
		telescopeEntries, tags := route.Handler(entry)
		t.Save(telescopeEntries, tags)
	}

	return nil
}

func (t *telescopeHook) Save(telescopeEntries *entries, tags []tag) {
	if telescopeEntries != nil {
		res := t.mysql.Table("telescope_entries").Create(telescopeEntries)
		if res.Error == nil {
			for _, tag := range tags {
				t.mysql.Table("telescope_entries_tags").Create(map[string]interface{}{
					"entry_uuid": tag.EntryUuid,
					"tag":        tag.Tag,
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
	// 如果有解密的系统, 直接设置这个值才回正常记录
	DecodeBody []byte
}

func (w TelescopeResponseWriter) Write(b []byte) (int, error) {
	w.Body.Write(b)
	return w.ResponseWriter.Write(b)
}

func (w TelescopeResponseWriter) WriteString(s string) (int, error) {
	w.Body.WriteString(s)
	return w.ResponseWriter.WriteString(s)
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
var EntryTypeTCP = "tcp"
var EntryTypeSCHEDULED_TASK = "schedule"
var EntryTypeGATE = "gate"
var EntryTypeVIEW = "view"
var EntryTypeCLIENT_REQUEST = "home_request"
