package main

import (
	"bytes"
	"encoding/json"
	"fmt"
	"golang.org/x/net/html"
	"golang.org/x/text/encoding/simplifiedchinese"
	"golang.org/x/text/transform"
	"io/ioutil"
	"log"
	"net/http"
	"net/url"
	"os"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"time"
	"unsafe"
)

const INT_SIZE = int(unsafe.Sizeof(int(0)))

var (
	homeURL        = GetEnv("HOME_URL", "https://www.biquge.cm")
	chapterPageURL = GetEnv("CHAPTER_PAGE_URL", "https://www.biquge.cm/6/6388/")
	dingBotUrl     = GetEnv("DING_BOT_URL", "")
	dbPath         = GetEnv("DB_PATH", "./info.db")
	initChapter    = GetEnvInt("INIT_CHAPTER", 3049)
	novelName      = GetEnv("NOVEL_NAME", "全职法师")
	record         *Record
)

func main() {
	var err error
	record, err = NewRecord(dbPath, initChapter)
	if err != nil {
		log.Fatal(err)
	}

	defer record.Close()

	for {
		checkUpdate()
		time.Sleep(time.Minute * 5)
	}
}

var httpClient = http.Client{
	Timeout: time.Second * 10,
}

func checkUpdate() {
	log.Println("checking...")
	resp, err := httpClient.Get(chapterPageURL)
	if err != nil {
		log.Printf("failed to request chapter page: %s", err.Error())
		return
	}
	defer resp.Body.Close()

	doc, err := html.Parse(resp.Body)
	if err != nil {
		log.Printf("failed to parse chapter page: %s", err.Error())
		return
	}

	listNode := findNode(doc, nodePath)
	if listNode == nil {
		log.Printf("failed to find list node")
		return
	}

	var (
		chapters []string
		hrefs    []string
		cur      = record.Get()
		latest   int
	)

	forEachPost(listNode, func(c *html.Node) bool {
		if c.Type != html.ElementNode || c.Data != "dd" {
			return true
		}

		textNode := findNode(c, chapterPath)
		if textNode == nil {
			return true
		}

		aNode := textNode.Parent

		chapter := strings.TrimSpace(textNode.Data)

		var id = 0

		_chapter := gbkToUtf8(chapter)
		id, _ = strconv.Atoi(strings.TrimPrefix(strings.TrimSuffix(strings.Split(_chapter, " ")[0], "章"), "第"))
		if id > 0 {
			chapter = _chapter
		} else {
			id, _ = strconv.Atoi(strings.TrimPrefix(strings.TrimSuffix(strings.Split(chapter, " ")[0], "章"), "第"))
		}

		if id == cur {
			return false
		}

		chapters = append(chapters, chapter)
		hrefs = append(hrefs, getAttr(aNode, "href"))
		if id > latest {
			latest = id
		}
		return true
	})

	if len(chapters) > 0 {
		record.Set(latest)
		notify(latest, chapters, hrefs)
	}

}

func notify(latest int, chapters, hrefs []string) {
	buf := bytes.NewBuffer(nil)
	buf.WriteString(fmt.Sprintf("<<%s>> 已经更新到第%d章\n", novelName, latest))
	buf.WriteString(fmt.Sprintf("最新章节为 %s\n", chapters[0]))
	buf.WriteString(fmt.Sprintf("最新章节列表："))
	for i := range chapters {
		u, _ := url.Parse(homeURL)
		u.Path = hrefs[i]
		buf.WriteString(fmt.Sprintf("\n%s：%s", chapters[i], u.String()))
	}
	msg := Message{}
	msg.Text.Content = buf.String()
	sendMessage(msg)
}

var chapterPath = []Step{
	{
		Type: html.ElementNode,
		Data: "a",
	},
	{
		Type: html.TextNode,
	},
}
var nodePath = []Step{
	{
		Type: html.ElementNode,
		Data: "html",
	},
	{
		Type: html.ElementNode,
		Data: "body",
	},
	{
		Type: html.ElementNode,
		Data: "div",
		Attrs: map[string]string{
			"id": "wrapper",
		},
	},
	{
		Type: html.ElementNode,
		Data: "div",
		Attrs: map[string]string{
			"class": "box_con",
		},
		Post: true,
	},
	{
		Type: html.ElementNode,
		Data: "div",
		Attrs: map[string]string{
			"id": "list",
		},
	},
	{
		Type: html.ElementNode,
		Data: "dl",
	},
}

type Step struct {
	Type  html.NodeType
	Data  string
	Attrs map[string]string
	Post  bool
}

func getAttr(n *html.Node, key string) string {
	for _, attr := range n.Attr {
		if attr.Key == key {
			return attr.Val
		}
	}
	return ""
}

func findNode(n *html.Node, steps []Step) *html.Node {
	var (
		cur        = n
		travelFunc func(*html.Node, func(*html.Node) bool) *html.Node
	)

	for _, s := range steps {
		if cur == nil {
			break
		}

		if s.Post {
			travelFunc = forEachPost
		} else {
			travelFunc = forEach
		}

		cur = travelFunc(cur, func(c *html.Node) bool {
			if c.Type == s.Type {
				if s.Data != "" && s.Data != c.Data {
					return true
				}

				if len(s.Attrs) > 0 {
					if len(s.Attrs) > len(c.Attr) {
						return true
					}

					for k, v := range s.Attrs {
						if getAttr(c, k) != v {
							return true
						}
					}
				}

				return false
			}
			return true
		})

	}

	return cur
}

func forEach(n *html.Node, f func(*html.Node) bool) *html.Node {
	if n == nil {
		return nil
	}
	for c := n.FirstChild; c != nil; c = c.NextSibling {
		if !f(c) {
			return c
		}
	}
	return nil
}

func forEachPost(n *html.Node, f func(*html.Node) bool) *html.Node {
	if n == nil {
		return nil
	}
	for c := n.LastChild; c != nil; c = c.PrevSibling {
		if !f(c) {
			return c
		}
	}
	return nil
}

type Record struct {
	path   string
	file   *os.File
	len    int
	latest *int
	mu     sync.Mutex
}

func NewRecord(path string, def int) (*Record, error) {
	fd, err := os.OpenFile(path, os.O_RDWR|os.O_CREATE, os.ModePerm)
	if err != nil {
		return nil, err
	}

	info, err := fd.Stat()
	if err != nil {
		fd.Close()
		return nil, err
	}

	if info.Size() != int64(INT_SIZE) {
		err = fd.Truncate(int64(INT_SIZE))
		if err != nil {
			fd.Close()
			return nil, err
		}
	}

	buf, err := syscall.Mmap(int(fd.Fd()), 0, INT_SIZE, syscall.PROT_WRITE|syscall.PROT_READ, syscall.MAP_SHARED)
	if err != nil {
		fd.Close()
		return nil, err
	}

	l := &Record{
		path: path,
		file: fd,
		len:  len(buf),
	}

	l.latest = (*int)(unsafe.Pointer(&buf[0]))

	if *l.latest == 0 {
		*l.latest = def
		l.sync()
	}

	return l, nil
}

func (l *Record) Set(latest int) {
	l.mu.Lock()
	defer l.mu.Unlock()
	*l.latest = latest
	l.sync()
}

func (l *Record) Get() int {
	l.mu.Lock()
	defer l.mu.Unlock()
	return *l.latest
}

func (l *Record) Close() {
	l.mu.Lock()
	defer l.mu.Unlock()

	l.sync()

	_, _, errno := syscall.Syscall(syscall.SYS_MUNMAP, uintptr(unsafe.Pointer(l.latest)), uintptr(l.len), 0)
	if errno != 0 {
		log.Printf("failed to unmmap file: %s", syscall.Errno(errno).Error())
	}
	if err := l.file.Close(); err != nil {
		log.Printf("failed to close file: %s", err.Error())
	}

	l.latest = nil
	l.len = 0
	l.file = nil
}

func (l *Record) sync() {
	_, _, errno := syscall.Syscall(syscall.SYS_MSYNC, uintptr(unsafe.Pointer(l.latest)), uintptr(l.len), syscall.MS_SYNC)
	if errno != 0 {
		log.Printf("failed to sync mmap memory to file: %s", syscall.Errno(errno).Error())
	}
}

type Message struct {
	MsgType string `json:"msgtype"`
	Text    struct {
		Content string `json:"content"`
	} `json:"text"`
}

func sendMessage(msg Message) {
	if msg.MsgType == "" {
		msg.MsgType = "text"
	}
	byts, _ := json.Marshal(msg)
	buf := bytes.NewBuffer(byts)
	resp, err := httpClient.Post(dingBotUrl, "application/json", buf)
	if err != nil {
		log.Printf("failed to send dingbot message: %s", err.Error())
		return
	}
	defer resp.Body.Close()
	if resp.StatusCode < 200 || resp.StatusCode > 399 {
		log.Printf("failed to send dingbot message: [%d]%s", resp.StatusCode, resp.Status)
	}
}

func GetEnv(key, def string) string {
	val := os.Getenv(key)
	if val == "" {
		return def
	}
	return val
}

func GetEnvInt(key string, def int) int {
	val, _ := strconv.Atoi(os.Getenv(key))
	if val > 0 {
		return val
	}
	return def
}

func gbkToUtf8(s string) string {
	reader := transform.NewReader(bytes.NewBufferString(s), simplifiedchinese.GBK.NewDecoder())
	d, e := ioutil.ReadAll(reader)
	if e != nil {
		return s
	}
	return string(d)
}
