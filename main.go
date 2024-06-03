package main

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"github.com/gin-gonic/gin"
	"github.com/tidwall/gjson"
	"github.com/tidwall/sjson"
	"golang.org/x/net/http2"
	"io"
	"log"
	"net/http"
	"net/url"
	"os"
	"reflect"
	"strconv"
	"strings"
	"time"
)

const DefaultInstructModel = "gpt-3.5-turbo-instruct"

const StableCodeModelPrefix = "stable-code"

type config struct {
	Bind                 string            `json:"bind"`
	ProxyUrl             string            `json:"proxy_url"`
	Timeout              int               `json:"timeout"`
	CodexApiBase         string            `json:"codex_api_base"`
	CodexApiKey          string            `json:"codex_api_key"`
	CodexApiOrganization string            `json:"codex_api_organization"`
	CodexApiProject      string            `json:"codex_api_project"`
	CodeInstructModel    string            `json:"code_instruct_model"`
	ChatApiBase          string            `json:"chat_api_base"`
	ChatApiKey           string            `json:"chat_api_key"`
	ChatApiOrganization  string            `json:"chat_api_organization"`
	ChatApiProject       string            `json:"chat_api_project"`
	ChatMaxTokens        int               `json:"chat_max_tokens"`
	ChatModelDefault     string            `json:"chat_model_default"`
	ChatModelMap         map[string]string `json:"chat_model_map"`
	ChatLocale           string            `json:"chat_locale"`
	AuthToken            string            `json:"auth_token"`
}

type Message struct {
	Role    string  `json:"role,omitempty"`
	Content any     `json:"content,omitempty"`
	Name    *string `json:"name,omitempty"`
}
type ChatCompletionsStreamResponseChoice struct {
	Index        int     `json:"index"`
	Delta        Message `json:"delta"`
	FinishReason *string `json:"finish_reason,omitempty"`
}

type ChatCompletionsStreamResponse struct {
	Id      string                                `json:"id"`
	Object  string                                `json:"object"`
	Created int64                                 `json:"created"`
	Model   string                                `json:"model"`
	Choices []ChatCompletionsStreamResponseChoice `json:"choices"`
}
type CustomEvent struct {
	Event string
	Id    string
	Retry uint
	Data  interface{}
}
type stringWriter interface {
	io.Writer
	writeString(string) (int, error)
}

type stringWrapper struct {
	io.Writer
}

var dataReplacer = strings.NewReplacer(
	"\n", "\ndata:",
	"\r", "\\r")
var contentType = []string{"text/event-stream"}
var noCache = []string{"no-cache"}

func (w stringWrapper) writeString(str string) (int, error) {
	return w.Writer.Write([]byte(str))
}
func checkWriter(writer io.Writer) stringWriter {
	if w, ok := writer.(stringWriter); ok {
		return w
	} else {
		return stringWrapper{writer}
	}
}
func encode(writer io.Writer, event CustomEvent) error {
	w := checkWriter(writer)
	return writeData(w, event.Data)
}
func writeData(w stringWriter, data interface{}) error {
	dataReplacer.WriteString(w, fmt.Sprint(data))
	if strings.HasPrefix(data.(string), "data") {
		w.writeString("\n\n")
	}
	return nil
}
func (r CustomEvent) Render(w http.ResponseWriter) error {
	r.WriteContentType(w)
	return encode(w, r)
}

func (r CustomEvent) WriteContentType(w http.ResponseWriter) {
	header := w.Header()
	header["Content-Type"] = contentType

	if _, exist := header["Cache-Control"]; !exist {
		header["Cache-Control"] = noCache
	}
}

func GetTimestamp() int64 {
	return time.Now().Unix()
}

func SetEventStreamHeaders(c *gin.Context) {
	c.Writer.Header().Set("Content-Type", "text/event-stream")
	c.Writer.Header().Set("Cache-Control", "no-cache")
	c.Writer.Header().Set("Connection", "keep-alive")
	c.Writer.Header().Set("Transfer-Encoding", "chunked")
	c.Writer.Header().Set("X-Accel-Buffering", "no")
}

const (
	RequestIdKey = "X-Oneapi-Request-Id"
)

func GetResponseID(c *gin.Context) string {
	logID := c.GetString(RequestIdKey)
	return fmt.Sprintf("chatcmpl-%s", logID)
}

func readConfig() *config {
	content, err := os.ReadFile("config.json")
	if nil != err {
		log.Fatal(err)
	}

	_cfg := &config{}
	err = json.Unmarshal(content, &_cfg)
	if nil != err {
		log.Fatal(err)
	}

	v := reflect.ValueOf(_cfg).Elem()
	t := v.Type()

	for i := 0; i < v.NumField(); i++ {
		field := v.Field(i)
		tag := t.Field(i).Tag.Get("json")
		if tag == "" {
			continue
		}

		value, exists := os.LookupEnv("OVERRIDE_" + strings.ToUpper(tag))
		if !exists {
			continue
		}

		switch field.Kind() {
		case reflect.String:
			field.SetString(value)
		case reflect.Bool:
			if boolValue, err := strconv.ParseBool(value); err == nil {
				field.SetBool(boolValue)
			}
		case reflect.Int, reflect.Int8, reflect.Int16, reflect.Int32, reflect.Int64:
			if intValue, err := strconv.ParseInt(value, 10, 64); err == nil {
				field.SetInt(intValue)
			}
		case reflect.Uint, reflect.Uint8, reflect.Uint16, reflect.Uint32, reflect.Uint64, reflect.Uintptr:
			if uintValue, err := strconv.ParseUint(value, 10, 64); err == nil {
				field.SetUint(uintValue)
			}
		case reflect.Float32, reflect.Float64:
			if floatValue, err := strconv.ParseFloat(value, field.Type().Bits()); err == nil {
				field.SetFloat(floatValue)
			}
		}
	}
	if _cfg.CodeInstructModel == "" {
		_cfg.CodeInstructModel = DefaultInstructModel
	}

	return _cfg
}

func getClient(cfg *config) (*http.Client, error) {
	transport := &http.Transport{
		ForceAttemptHTTP2: true,
		DisableKeepAlives: false,
	}

	err := http2.ConfigureTransport(transport)
	if nil != err {
		return nil, err
	}

	if "" != cfg.ProxyUrl {
		proxyUrl, err := url.Parse(cfg.ProxyUrl)
		if nil != err {
			return nil, err
		}

		transport.Proxy = http.ProxyURL(proxyUrl)
	}

	client := &http.Client{
		Transport: transport,
		Timeout:   time.Duration(cfg.Timeout) * time.Second,
	}

	return client, nil
}

func abortCodex(c *gin.Context, status int) {
	c.Header("Content-Type", "text/event-stream")

	c.String(status, "data: [DONE]\n")
	c.Abort()
}

func closeIO(c io.Closer) {
	err := c.Close()
	if nil != err {
		log.Println(err)
	}
}

type ProxyService struct {
	cfg    *config
	client *http.Client
}

func NewProxyService(cfg *config) (*ProxyService, error) {
	client, err := getClient(cfg)
	if nil != err {
		return nil, err
	}

	return &ProxyService{
		cfg:    cfg,
		client: client,
	}, nil
}
func AuthMiddleware(authToken string) gin.HandlerFunc {
	return func(c *gin.Context) {
		token := c.Param("token")
		if token != authToken {
			c.JSON(http.StatusUnauthorized, gin.H{"error": "Unauthorized"})
			c.Abort()
			return
		}
		c.Next()
	}
}

func (s *ProxyService) InitRoutes(e *gin.Engine) {
	authToken := s.cfg.AuthToken // replace with your dynamic value as needed
	if authToken != "" {
		// 鉴权
		v1 := e.Group("/:token/v1/", AuthMiddleware(authToken))
		{
			v1.POST("/chat/completions", s.completions)
			v1.POST("/engines/copilot-codex/completions", s.codeCompletions)
		}
	} else {
		e.POST("/v1/chat/completions", s.completions)
		e.POST("/v1/engines/copilot-codex/completions", s.codeCompletions)
	}
}

func (s *ProxyService) completions(c *gin.Context) {
	ctx := c.Request.Context()

	body, err := io.ReadAll(c.Request.Body)
	if nil != err {
		c.AbortWithStatus(http.StatusBadRequest)
		return
	}

	model := gjson.GetBytes(body, "model").String()
	if mapped, ok := s.cfg.ChatModelMap[model]; ok {
		model = mapped
	} else {
		model = s.cfg.ChatModelDefault
	}
	body, _ = sjson.SetBytes(body, "model", model)

	if !gjson.GetBytes(body, "function_call").Exists() {
		messages := gjson.GetBytes(body, "messages").Array()
		lastIndex := len(messages) - 1
		if !strings.Contains(messages[lastIndex].Get("content").String(), "Respond in the following locale") {
			locale := s.cfg.ChatLocale
			if locale == "" {
				locale = "zh_CN"
			}
			body, _ = sjson.SetBytes(body, "messages."+strconv.Itoa(lastIndex)+".content", messages[lastIndex].Get("content").String()+"Respond in the following locale: "+locale+".")
		}
	}

	body, _ = sjson.DeleteBytes(body, "intent")
	body, _ = sjson.DeleteBytes(body, "intent_threshold")
	body, _ = sjson.DeleteBytes(body, "intent_content")

	if int(gjson.GetBytes(body, "max_tokens").Int()) > s.cfg.ChatMaxTokens {
		body, _ = sjson.SetBytes(body, "max_tokens", s.cfg.ChatMaxTokens)
	}

	proxyUrl := s.cfg.ChatApiBase + "/chat/completions"
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, proxyUrl, io.NopCloser(bytes.NewBuffer(body)))
	if nil != err {
		c.AbortWithStatus(http.StatusInternalServerError)
		return
	}

	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "Bearer "+s.cfg.ChatApiKey)
	if "" != s.cfg.ChatApiOrganization {
		req.Header.Set("OpenAI-Organization", s.cfg.ChatApiOrganization)
	}
	if "" != s.cfg.ChatApiProject {
		req.Header.Set("OpenAI-Project", s.cfg.ChatApiProject)
	}

	resp, err := s.client.Do(req)
	if nil != err {
		if errors.Is(err, context.Canceled) {
			c.AbortWithStatus(http.StatusRequestTimeout)
			return
		}

		log.Println("request conversation failed:", err.Error())
		c.AbortWithStatus(http.StatusInternalServerError)
		return
	}
	defer closeIO(resp.Body)

	if resp.StatusCode != http.StatusOK { // log
		body, _ := io.ReadAll(resp.Body)
		log.Println("request completions failed:", string(body))

		resp.Body = io.NopCloser(bytes.NewBuffer(body))
	}

	c.Status(resp.StatusCode)

	contentType := resp.Header.Get("Content-Type")
	if "" != contentType {
		c.Header("Content-Type", contentType)
	}

	_, _ = io.Copy(c.Writer, resp.Body)
}

func (s *ProxyService) codeCompletions(c *gin.Context) {
	ctx := c.Request.Context()

	time.Sleep(100 * time.Millisecond)
	if ctx.Err() != nil {
		abortCodex(c, http.StatusRequestTimeout)
		return
	}

	body, err := io.ReadAll(c.Request.Body)
	if nil != err {
		abortCodex(c, http.StatusBadRequest)
		return
	}

	body = ConstructRequestBody(body, s.cfg)

	proxyUrl := s.cfg.CodexApiBase + "/completions"
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, proxyUrl, io.NopCloser(bytes.NewBuffer(body)))
	if nil != err {
		//
		abortCodex(c, http.StatusInternalServerError)
		return
	}

	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "Bearer "+s.cfg.CodexApiKey)
	if "" != s.cfg.CodexApiOrganization {
		req.Header.Set("OpenAI-Organization", s.cfg.CodexApiOrganization)
	}
	if "" != s.cfg.CodexApiProject {
		req.Header.Set("OpenAI-Project", s.cfg.CodexApiProject)
	}

	resp, err := s.client.Do(req)
	if nil != err {
		if errors.Is(err, context.Canceled) {
			abortCodex(c, http.StatusRequestTimeout)
			return
		}

		log.Println("request completions failed:", err.Error())
		abortCodex(c, http.StatusInternalServerError)
		return
	}
	defer closeIO(resp.Body)

	if resp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(resp.Body)
		log.Println("request completions failed:", string(body))

		abortCodex(c, resp.StatusCode)
		return
	}

	c.Status(resp.StatusCode)

	contentType := resp.Header.Get("Content-Type")
	if "" != contentType {
		c.Header("Content-Type", contentType)
	}

	//_, _ = io.Copy(c.Writer, resp.Body)
	scanner := bufio.NewScanner(resp.Body)
	scanner.Split(func(data []byte, atEOF bool) (advance int, token []byte, err error) {
		if atEOF && len(data) == 0 {
			return 0, nil, nil
		}
		if i := bytes.IndexByte(data, '\n'); i >= 0 {
			return i + 1, data[0:i], nil
		}
		if atEOF {
			return len(data), data, nil
		}
		return 0, nil, nil
	})
	dataChan := make(chan string)
	stopChan := make(chan bool)
	go func() {
		for scanner.Scan() {
			data := scanner.Text()
			if len(data) < len("data: ") {
				continue
			}
			data = strings.TrimPrefix(data, "data: ")
			dataChan <- data
		}
		stopChan <- true
	}()
	SetEventStreamHeaders(c)
	id := GetResponseID(c)
	//responseModel := c.GetString("original_model")
	c.Stream(func(w io.Writer) bool {
		select {
		case data := <-dataChan:
			// some implementations may add \r at the end of data
			data = strings.TrimSuffix(data, "\r")
			var codeResponse ChatCompletionsStreamResponse
			err := json.Unmarshal([]byte(data), &codeResponse)
			if err != nil {
				if data == "[DONE]" {
					return true
				}
				log.Println("error unmarshalling stream response: ", err.Error())
				return true
			}
			if strings.HasPrefix(s.cfg.CodeInstructModel, "@") {
				//for _, choiceData := range codeResponse.Choices {
				//	choiceData.Index = 1
				//}
				if codeResponse.Choices[0].Delta.Content == "<｜end▁of▁sentence｜>" {
					codeResponse.Choices[0].Delta.Content = ""
				}
				jsonStr, err := json.Marshal(codeResponse)
				if err != nil {
					log.Println("error marshalling stream response: ", err.Error())
					return true
				}
				c.Render(-1, CustomEvent{Data: "data: " + string(jsonStr)})
			} else {
				c.Render(-1, CustomEvent{Data: "data:" + string(data)})
			}
			return true
		case <-stopChan:
			endStr := `stop`
			endChoises := ChatCompletionsStreamResponseChoice{
				FinishReason: &endStr,
			}
			endResponse := ChatCompletionsStreamResponse{
				Id:      id,
				Object:  "chat.completion.chunk",
				Created: GetTimestamp(),
				Model:   s.cfg.CodeInstructModel,
				Choices: []ChatCompletionsStreamResponseChoice{},
			}
			endResponse.Choices = append(endResponse.Choices, endChoises)
			jsonStr, err := json.Marshal(endResponse)
			if err != nil {
				log.Println("error marshalling stream response: ", err.Error())
				return true
			}
			c.Render(-1, CustomEvent{Data: "data: " + string(jsonStr)})
			c.Render(-1, CustomEvent{Data: "data: [DONE]"})
			return false
		}
	})
	_ = resp.Body.Close()
}

func ConstructRequestBody(body []byte, cfg *config) []byte {
	body, _ = sjson.DeleteBytes(body, "extra")
	body, _ = sjson.DeleteBytes(body, "nwo")
	body, _ = sjson.SetBytes(body, "model", cfg.CodeInstructModel)
	if strings.Contains(cfg.CodeInstructModel, StableCodeModelPrefix) {
		return constructWithStableCodeModel(body)
	} else if strings.HasPrefix(cfg.CodeInstructModel, "@") {
		return constructWithCfCodeModel(body)
	}
	if strings.HasSuffix(cfg.ChatApiBase, "chat") {
		// @Todo  constructWithChatModel
		// 如果code base以chat结尾则构建chatModel，暂时没有好的prompt
	}
	return body
}

func constructWithCfCodeModel(body []byte) []byte {
	suffix := gjson.GetBytes(body, "suffix")
	prompt := gjson.GetBytes(body, "prompt")
	content := fmt.Sprintf("<｜fim▁begin｜>%s<｜fim▁hole｜>%s<｜fim▁end｜>", prompt, suffix)

	// 创建新的 JSON 对象并添加到 body 中
	messages := []map[string]string{
		{
			"role":    "user",
			"content": content,
		},
	}
	return constructWithChatModel(body, messages)
}

func constructWithStableCodeModel(body []byte) []byte {
	suffix := gjson.GetBytes(body, "suffix")
	prompt := gjson.GetBytes(body, "prompt")
	content := fmt.Sprintf("<fim_prefix>%s<fim_suffix>%s<fim_middle>", prompt, suffix)

	// 创建新的 JSON 对象并添加到 body 中
	messages := []map[string]string{
		{
			"role":    "user",
			"content": content,
		},
	}
	return constructWithChatModel(body, messages)
}

func constructWithChatModel(body []byte, messages interface{}) []byte {

	body, _ = sjson.SetBytes(body, "messages", messages)

	// fmt.Printf("Request Body: %s\n", body)
	// 2. 将转义的字符替换回原来的字符
	jsonStr := string(body)
	jsonStr = strings.ReplaceAll(jsonStr, "\\u003c", "<")
	jsonStr = strings.ReplaceAll(jsonStr, "\\u003e", ">")
	return []byte(jsonStr)
}

func main() {
	cfg := readConfig()

	gin.SetMode(gin.ReleaseMode)
	r := gin.Default()

	proxyService, err := NewProxyService(cfg)
	if nil != err {
		log.Fatal(err)
		return
	}

	proxyService.InitRoutes(r)

	err = r.Run(cfg.Bind)
	if nil != err {
		log.Fatal(err)
		return
	}

}
