package spider

import (
	"io/ioutil"
	"mime"
	"net/http"
	"path"
	"strings"
	"sync"
	"time"

	"github.com/PuerkitoBio/goquery"
	"golang.org/x/net/html/charset"

	"github.com/henrylee2cn/pholcus/app/downloader/request"
	"github.com/henrylee2cn/pholcus/app/pipeline/collector/data"
	"github.com/henrylee2cn/pholcus/common/util"
	"github.com/henrylee2cn/pholcus/logs"
)

type Context struct {
	spider   *Spider           // 规则
	Request  *request.Request  // 原始请求
	Response *http.Response    // 响应流，其中URL拷贝自*request.Request
	text     string            // 下载内容Body的字符串格式
	dom      *goquery.Document // 下载内容Body为html时，可转换为Dom的对象
	items    []data.DataCell   // 存放以文本形式输出的结果数据
	files    []data.FileCell   // 存放欲直接输出的文件("Name": string; "Body": io.ReadCloser)
	err      error             // 错误标记
	sync.Mutex
}

var (
	contextPool = &sync.Pool{
		New: func() interface{} {
			return &Context{
				items: []data.DataCell{},
				files: []data.FileCell{},
			}
		},
	}
)

//**************************************** 初始化 *******************************************\\

func GetContext(sp *Spider, req *request.Request) *Context {
	ctx := contextPool.Get().(*Context)
	ctx.spider = sp
	ctx.Request = req
	return ctx
}

func PutContext(ctx *Context) {
	ctx.items = ctx.items[:0]
	ctx.files = ctx.files[:0]
	ctx.spider = nil
	ctx.Request = nil
	ctx.Response = nil
	ctx.text = ""
	ctx.dom = nil
	ctx.err = nil
	contextPool.Put(ctx)
}

func (self *Context) SetResponse(resp *http.Response) *Context {
	self.Response = resp
	return self
}

// 标记下载错误。
func (self *Context) SetError(err error) {
	self.err = err
}

//**************************************** Set与Exec类公开方法 *******************************************\\

// 生成并添加请求至队列。
// Request.Url与Request.Rule必须设置。
// Request.Spider无需手动设置(由系统自动设置)。
// Request.EnableCookie在Spider字段中统一设置，规则请求中指定的无效。
// 以下字段有默认值，可不设置:
// Request.Method默认为GET方法;
// Request.DialTimeout默认为常量request.DefaultDialTimeout，小于0时不限制等待响应时长;
// Request.ConnTimeout默认为常量request.DefaultConnTimeout，小于0时不限制下载超时;
// Request.TryTimes默认为常量request.DefaultTryTimes，小于0时不限制失败重载次数;
// Request.RedirectTimes默认不限制重定向次数，小于0时可禁止重定向跳转;
// Request.RetryPause默认为常量request.DefaultRetryPause;
// Request.DownloaderID指定下载器ID，0为默认的Surf高并发下载器，功能完备，1为PhantomJS下载器，特点破防力强，速度慢，低并发。
// 默认自动补填Referer。
func (self *Context) AddQueue(req *request.Request) *Context {
	// 若已主动终止任务，则崩溃爬虫协程
	self.spider.tryPanic()

	err := req.
		SetSpiderName(self.spider.GetName()).
		SetEnableCookie(self.spider.GetEnableCookie()).
		Prepare()

	if err != nil {
		logs.Log.Error(err.Error())
		return self
	}

	// 自动设置Referer
	if req.GetReferer() == "" && self.Response != nil {
		req.SetReferer(self.GetUrl())
	}

	self.spider.RequestPush(req)
	return self
}

// 用于动态规则添加请求。
func (self *Context) JsAddQueue(jreq map[string]interface{}) *Context {
	// 若已主动终止任务，则崩溃爬虫协程
	self.spider.tryPanic()

	req := &request.Request{}
	u, ok := jreq["Url"].(string)
	if !ok {
		return self
	}
	req.Url = u
	req.Rule, _ = jreq["Rule"].(string)
	req.Method, _ = jreq["Method"].(string)
	req.Header = http.Header{}
	if header, ok := jreq["Header"].(map[string]interface{}); ok {
		for k, values := range header {
			if vals, ok := values.([]string); ok {
				for _, v := range vals {
					req.Header.Add(k, v)
				}
			}
		}
	}
	req.PostData, _ = jreq["PostData"].(string)
	req.Reloadable, _ = jreq["Reloadable"].(bool)
	if t, ok := jreq["DialTimeout"].(int64); ok {
		req.DialTimeout = time.Duration(t)
	}
	if t, ok := jreq["ConnTimeout"].(int64); ok {
		req.ConnTimeout = time.Duration(t)
	}
	if t, ok := jreq["RetryPause"].(int64); ok {
		req.RetryPause = time.Duration(t)
	}
	if t, ok := jreq["TryTimes"].(int64); ok {
		req.TryTimes = int(t)
	}
	if t, ok := jreq["RedirectTimes"].(int64); ok {
		req.RedirectTimes = int(t)
	}
	if t, ok := jreq["Priority"].(int64); ok {
		req.Priority = int(t)
	}
	if t, ok := jreq["DownloaderID"].(int64); ok {
		req.DownloaderID = int(t)
	}
	if t, ok := jreq["Temp"].(map[string]interface{}); ok {
		req.Temp = t
	}

	err := req.
		SetSpiderName(self.spider.GetName()).
		SetEnableCookie(self.spider.GetEnableCookie()).
		Prepare()

	if err != nil {
		logs.Log.Error(err.Error())
		return self
	}

	if req.GetReferer() == "" && self.Response != nil {
		req.SetReferer(self.GetUrl())
	}

	self.spider.RequestPush(req)
	return self
}

// 输出文本结果。
// item类型为map[int]interface{}时，根据ruleName现有的ItemFields字段进行输出，
// item类型为map[string]interface{}时，ruleName不存在的ItemFields字段将被自动添加，
// ruleName为空时默认当前规则。
func (self *Context) Output(item interface{}, ruleName ...string) {
	_ruleName, rule, found := self.getRule(ruleName...)
	if !found {
		logs.Log.Error("蜘蛛 %s 调用Output()时，指定的规则名不存在！", self.spider.GetName())
		return
	}
	var _item map[string]interface{}
	switch item2 := item.(type) {
	case map[int]interface{}:
		_item = self.CreatItem(item2, _ruleName)
	case request.Temp:
		for k := range item2 {
			self.spider.UpsertItemField(rule, k)
		}
		_item = item2
	case map[string]interface{}:
		for k := range item2 {
			self.spider.UpsertItemField(rule, k)
		}
		_item = item2
	}
	self.Lock()
	if self.spider.NotDefaultField {
		self.items = append(self.items, data.GetDataCell(_ruleName, _item, "", "", ""))
	} else {
		self.items = append(self.items, data.GetDataCell(_ruleName, _item, self.GetUrl(), self.GetReferer(), time.Now().Format("2006-01-02 15:04:05")))
	}
	self.Unlock()
}

// 输出文件。
// name指定文件名，为空时默认保持原文件名不变。
func (self *Context) FileOutput(name ...string) {
	// 读取完整文件流
	bytes, err := ioutil.ReadAll(self.Response.Body)
	self.Response.Body.Close()
	if err != nil {
		panic(err.Error())
		return
	}

	// 智能设置完整文件名
	_, s := path.Split(self.GetUrl())
	n := strings.Split(s, "?")[0]

	baseName := strings.Split(n, ".")[0]
	ext := path.Ext(n)

	if len(name) > 0 {
		p, n := path.Split(name[0])
		if baseName2 := strings.Split(n, ".")[0]; baseName2 != "" {
			baseName = p + baseName2
		}
		if ext == "" {
			ext = path.Ext(n)
		}
	}

	if ext == "" {
		ext = ".html"
	}

	// 保存到文件临时队列
	self.Lock()
	self.files = append(self.files, data.GetFileCell(self.GetRuleName(), baseName+ext, bytes))
	self.Unlock()
}

// 生成文本结果。
// 用ruleName指定匹配的ItemFields字段，为空时默认当前规则。
func (self *Context) CreatItem(item map[int]interface{}, ruleName ...string) map[string]interface{} {
	_, rule, found := self.getRule(ruleName...)
	if !found {
		logs.Log.Error("蜘蛛 %s 调用CreatItem()时，指定的规则名不存在！", self.spider.GetName())
		return nil
	}

	var item2 = make(map[string]interface{}, len(item))
	for k, v := range item {
		field := self.spider.GetItemField(rule, k)
		item2[field] = v
	}
	return item2
}

// 获得一个原始请求的副本。
func (self *Context) CopyRequest() *request.Request {
	return self.Request.Copy()
}

// 获得一个请求的缓存数据副本。
func (self *Context) CopyTemps() request.Temp {
	temps := make(request.Temp)
	for k, v := range self.Request.GetTemps() {
		temps[k] = v
	}
	return temps
}

// 在请求中保存临时数据。
func (self *Context) SetTemp(key string, value interface{}) *Context {
	self.Request.SetTemp(key, value)
	return self
}

func (self *Context) SetUrl(url string) *Context {
	self.Request.Url = url
	return self
}

func (self *Context) SetReferer(referer string) *Context {
	self.Request.Header.Set("Referer", referer)
	return self
}

// 为指定Rule动态追加结果字段名，并获取索引位置，
// 已存在时获取原来索引位置，
// 若ruleName为空，默认为当前规则。
func (self *Context) UpsertItemField(field string, ruleName ...string) (index int) {
	_, rule, found := self.getRule(ruleName...)
	if !found {
		logs.Log.Error("蜘蛛 %s 调用UpsertItemField()时，指定的规则名不存在！", self.spider.GetName())
		return
	}
	return self.spider.UpsertItemField(rule, field)
}

// 调用指定Rule下辅助函数AidFunc()。
// 用ruleName指定匹配的AidFunc，为空时默认当前规则。
func (self *Context) Aid(aid map[string]interface{}, ruleName ...string) interface{} {
	// 若已主动终止任务，则崩溃爬虫协程
	self.spider.tryPanic()

	_, rule, found := self.getRule(ruleName...)
	if !found {
		logs.Log.Error("蜘蛛 %s 调用Aid()时，指定的规则名不存在！", self.spider.GetName())
		return nil
	}

	return rule.AidFunc(self, aid)
}

// 解析响应流。
// 用ruleName指定匹配的ParseFunc字段，为空时默认调用Root()。
func (self *Context) Parse(ruleName ...string) *Context {
	// 若已主动终止任务，则崩溃爬虫协程
	self.spider.tryPanic()

	_ruleName, rule, found := self.getRule(ruleName...)
	if self.Response != nil {
		self.Request.SetRuleName(_ruleName)
	}
	if !found {
		self.spider.RuleTree.Root(self)
		return self
	}
	rule.ParseFunc(self)
	return self
}

// 设置自定义配置。
func (self *Context) SetKeyin(keyin string) *Context {
	self.spider.SetKeyin(keyin)
	return self
}

// 设置采集上限。
func (self *Context) SetLimit(max int) *Context {
	self.spider.SetLimit(int64(max))
	return self
}

// 自定义暂停区间(随机: Pausetime/2 ~ Pausetime*2)，优先级高于外部传参。
// 当且仅当runtime[0]为true时可覆盖现有值。
func (self *Context) SetPausetime(pause int64, runtime ...bool) *Context {
	self.spider.SetPausetime(pause, runtime...)
	return self
}

// 设置定时器，
// @id为定时器唯一标识，
// @bell==nil时为倒计时器，此时@tol为睡眠时长，
// @bell!=nil时为闹铃，此时@tol用于指定醒来时刻（从now起遇到的第tol个bell）。
func (self *Context) SetTimer(id string, tol time.Duration, bell *Bell) bool {
	return self.spider.SetTimer(id, tol, bell)
}

// 启动定时器，并获取定时器是否可以继续使用。
func (self *Context) RunTimer(id string) bool {
	return self.spider.RunTimer(id)
}

// 重置下载的文本内容，
func (self *Context) ResetText(body string) *Context {
	self.text = body
	self.dom = nil
	return self
}

//**************************************** Get 类公开方法 *******************************************\\

// 获取下载错误。
func (self *Context) GetError() error {
	return self.err
}

// 获取蜘蛛名称。
func (self *Context) GetSpider() *Spider {
	return self.spider
}

// 获取响应流。
func (self *Context) GetResponse() *http.Response {
	return self.Response
}

// 获取响应状态码。
func (self *Context) GetStatusCode() int {
	return self.Response.StatusCode
}

// 获取原始请求。
func (self *Context) GetRequest() *request.Request {
	return self.Request
}

// 获取结果字段名列表。
func (self *Context) GetItemFields(ruleName ...string) []string {
	_, rule, found := self.getRule(ruleName...)
	if !found {
		logs.Log.Error("蜘蛛 %s 调用GetItemFields()时，指定的规则名不存在！", self.spider.GetName())
		return nil
	}
	return self.spider.GetItemFields(rule)
}

// 由索引下标获取结果字段名，不存在时获取空字符串，
// 若ruleName为空，默认为当前规则。
func (self *Context) GetItemField(index int, ruleName ...string) (field string) {
	_, rule, found := self.getRule(ruleName...)
	if !found {
		logs.Log.Error("蜘蛛 %s 调用GetItemField()时，指定的规则名不存在！", self.spider.GetName())
		return
	}
	return self.spider.GetItemField(rule, index)
}

// 由结果字段名获取索引下标，不存在时索引为-1，
// 若ruleName为空，默认为当前规则。
func (self *Context) GetItemFieldIndex(field string, ruleName ...string) (index int) {
	_, rule, found := self.getRule(ruleName...)
	if !found {
		logs.Log.Error("蜘蛛 %s 调用GetItemField()时，指定的规则名不存在！", self.spider.GetName())
		return
	}
	return self.spider.GetItemFieldIndex(rule, field)
}

func (self *Context) PullItems() (ds []data.DataCell) {
	self.Lock()
	ds = self.items
	self.items = []data.DataCell{}
	self.Unlock()
	return
}

func (self *Context) PullFiles() (fs []data.FileCell) {
	self.Lock()
	fs = self.files
	self.files = []data.FileCell{}
	self.Unlock()
	return
}

// 获取自定义配置。
func (self *Context) GetKeyin() string {
	return self.spider.GetKeyin()
}

// 获取采集上限。
func (self *Context) GetLimit() int {
	return int(self.spider.GetLimit())
}

// 获取蜘蛛名。
func (self *Context) GetName() string {
	return self.spider.GetName()
}

// 获取规则树。
func (self *Context) GetRules() map[string]*Rule {
	return self.spider.GetRules()
}

// 获取指定规则。
func (self *Context) GetRule(ruleName string) (*Rule, bool) {
	return self.spider.GetRule(ruleName)
}

// 获取当前规则名。
func (self *Context) GetRuleName() string {
	return self.Request.GetRuleName()
}

// 获取请求中指定缓存数据，
// 强烈建议数据接收者receive为指针类型，
// receive为空时，直接输出字符串。
func (self *Context) GetTemp(key string, receive interface{}) interface{} {
	return self.Request.GetTemp(key, receive)
}

// 从原始请求获取Url，从而保证请求前后的Url完全相等，且中文未被编码。
func (self *Context) GetUrl() string {
	return self.Request.Url
}

func (self *Context) GetMethod() string {
	return self.Request.GetMethod()
}

func (self *Context) GetHost() string {
	return self.Response.Request.URL.Host
}

// 获取响应头信息。
func (self *Context) GetHeader() http.Header {
	return self.Response.Header
}

// 获取请求头信息。
func (self *Context) GetRequestHeader() http.Header {
	return self.Response.Request.Header
}

func (self *Context) GetReferer() string {
	return self.Response.Request.Header.Get("Referer")
}

// 获取响应的Cookie。
func (self *Context) GetCookie() string {
	return self.Response.Header.Get("Set-Cookie")
}

// GetHtmlParser returns goquery object binded to target crawl result.
func (self *Context) GetDom() *goquery.Document {
	if self.dom == nil {
		self.initDom()
	}
	return self.dom
}

// GetBodyStr returns plain string crawled.
func (self *Context) GetText() string {
	if self.text == "" {
		self.initText()
	}
	return self.text
}

//**************************************** 私有方法 *******************************************\\

// 获取规则。
func (self *Context) getRule(ruleName ...string) (name string, rule *Rule, found bool) {
	if len(ruleName) == 0 {
		if self.Response == nil {
			return
		}
		name = self.GetRuleName()
	} else {
		name = ruleName[0]
	}
	rule, found = self.spider.GetRule(name)
	return
}

// GetHtmlParser returns goquery object binded to target crawl result.
func (self *Context) initDom() *goquery.Document {
	r := strings.NewReader(self.GetText())
	var err error
	self.dom, err = goquery.NewDocumentFromReader(r)
	if err != nil {
		logs.Log.Error(err.Error())
		panic(err.Error())
	}
	return self.dom
}

// GetBodyStr returns plain string crawled.
func (self *Context) initText() {
	// 采用surf内核下载时，尝试自动转码
	if self.Request.DownloaderID == request.SURF_ID {
		var contentType, pageEncode string
		// 优先从响应头读取编码类型
		contentType = self.Response.Header.Get("Content-Type")
		if _, params, err := mime.ParseMediaType(contentType); err == nil {
			if cs, ok := params["charset"]; ok {
				pageEncode = strings.ToLower(strings.TrimSpace(cs))
			}
		}
		// 响应头未指定编码类型时，从请求头读取
		if len(pageEncode) == 0 {
			contentType = self.Request.Header.Get("Content-Type")
			if _, params, err := mime.ParseMediaType(contentType); err == nil {
				if cs, ok := params["charset"]; ok {
					pageEncode = strings.ToLower(strings.TrimSpace(cs))
				}
			}
		}

		switch pageEncode {
		// 不做转码处理
		case "", "utf8", "utf-8", "unicode-1-1-utf-8":
		default:
			// 指定了编码类型，但不是utf8时，自动转码为utf8
			// get converter to utf-8
			// Charset auto determine. Use golang.org/x/net/html/charset. Get response body and change it to utf-8
			destReader, err := charset.NewReaderLabel(pageEncode, self.Response.Body)
			if err == nil {
				sorbody, err := ioutil.ReadAll(destReader)
				if err == nil {
					self.Response.Body.Close()
					self.text = util.Bytes2String(sorbody)
					return
				} else {
					logs.Log.Warning(" *     [convert][%v]: %v (ignore transcoding)\n", self.GetUrl(), err)
				}
			} else {
				logs.Log.Warning(" *     [convert][%v]: %v (ignore transcoding)\n", self.GetUrl(), err)
			}
		}
	}

	// 不做转码处理
	sorbody, err := ioutil.ReadAll(self.Response.Body)
	self.Response.Body.Close()
	if err != nil {
		panic(err.Error())
		return
	}
	self.text = util.Bytes2String(sorbody)
}
