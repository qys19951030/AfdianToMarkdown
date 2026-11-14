// Package crawltest 提供离线测试用的假爱发电服务与辅助函数，
// 不连真实站点、不依赖 cookie。
package crawltest

import (
	"AfdianToMarkdown/config"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"sync"
)

// FakePost 假文章（动态或作品集条目）
type FakePost struct {
	ID          string
	Title       string
	Content     string // HTML 正文
	Sn          string // publish_sn，最旧的一篇应为空串表示分页结束
	PublishTime int64
}

// FakeAlbum 假作品集，PostIDs 引用 FakeAfdian.Posts 中的文章 id
type FakeAlbum struct {
	ID      string
	Title   string
	PostIDs []string
}

// FakeProduct 假电铺商品
type FakeProduct struct {
	ID         string
	Name       string
	Desc       string
	Price      string
	UpdateTime int64
}

// FakeAfdian 假爱发电服务，实现采集流程用到的全部 API
type FakeAfdian struct {
	srv *httptest.Server

	mu       sync.Mutex
	UserID   string
	UrlSlug  string
	Posts    []FakePost // 动态列表（新的在前）
	Albums   []FakeAlbum
	Products []FakeProduct

	// FailDetail 中的 post_id 请求正文时返回 500
	FailDetail map[string]bool
	// FailOnce 中的「路径+查询」首次请求返回 500 后自动恢复（模拟中断）
	FailOnce map[string]bool

	requests []string
}

// NewFakeAfdian 启动假服务
func NewFakeAfdian() *FakeAfdian {
	f := &FakeAfdian{
		UserID:     "user-1",
		UrlSlug:    "alice",
		FailDetail: map[string]bool{},
		FailOnce:   map[string]bool{},
	}
	f.srv = httptest.NewServer(http.HandlerFunc(f.handle))
	return f
}

// Close 关闭假服务
func (f *FakeAfdian) Close() { f.srv.Close() }

// URL 返回假服务地址
func (f *FakeAfdian) URL() string { return f.srv.URL }

// NewConfig 生成指向假服务的配置
func (f *FakeAfdian) NewConfig(dataDir string, skipFailed bool) *config.Config {
	return &config.Config{
		Host:       "fake.local",
		HostUrl:    f.srv.URL,
		DataDir:    dataDir,
		SkipFailed: skipFailed,
	}
}

// RequestCount 统计 URL 包含 substr 的请求次数
func (f *FakeAfdian) RequestCount(urlSubstr string) int {
	f.mu.Lock()
	defer f.mu.Unlock()
	n := 0
	for _, r := range f.requests {
		if strings.Contains(r, urlSubstr) {
			n++
		}
	}
	return n
}

// DetailCount 返回正文请求次数
func (f *FakeAfdian) DetailCount() int {
	return f.RequestCount("/api/post/get-detail")
}

// ResetCounts 清空请求计数
func (f *FakeAfdian) ResetCounts() {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.requests = nil
}

func (f *FakeAfdian) record(url string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.requests = append(f.requests, url)
}

func (f *FakeAfdian) shouldFailOnce(key string) bool {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.FailOnce[key] {
		delete(f.FailOnce, key)
		return true
	}
	return false
}

func (f *FakeAfdian) findPost(id string) *FakePost {
	for i := range f.Posts {
		if f.Posts[i].ID == id {
			return &f.Posts[i]
		}
	}
	return nil
}

func writeJSON(w http.ResponseWriter, v any) {
	w.Header().Set("Content-Type", "application/json")
	data, _ := json.Marshal(v)
	_, _ = w.Write(data)
}

func (f *FakeAfdian) handle(w http.ResponseWriter, r *http.Request) {
	f.record(r.URL.String())
	key := r.URL.Path + "?" + r.URL.RawQuery
	if f.shouldFailOnce(key) {
		w.WriteHeader(http.StatusInternalServerError)
		return
	}
	q := r.URL.Query()
	switch r.URL.Path {
	case "/api/user/get-profile-by-slug":
		writeJSON(w, map[string]any{"data": map[string]any{"user": map[string]any{"user_id": f.UserID}}})
	case "/api/post/get-list":
		f.handleMotionList(w, q.Get("publish_sn"))
	case "/api/post/get-detail":
		f.handlePostDetail(w, q.Get("post_id"))
	case "/api/comment/get-list":
		writeJSON(w, map[string]any{"data": map[string]any{"list": []any{}, "hot_list": []any{}}})
	case "/api/user/get-album-list":
		f.handleAlbumList(w)
	case "/api/user/get-album-info":
		f.handleAlbumInfo(w, q.Get("album_id"))
	case "/api/user/get-album-post":
		f.handleAlbumPost(w, q.Get("album_id"), q.Get("lastRank"))
	case "/api/creator/get-products":
		f.handleProducts(w, q.Get("page"))
	default:
		w.WriteHeader(http.StatusNotFound)
	}
}

func postListItem(p FakePost) map[string]any {
	return map[string]any{
		"post_id":      p.ID,
		"title":        p.Title,
		"content":      p.Content,
		"publish_time": strconv.FormatInt(p.PublishTime, 10),
		"publish_sn":   p.Sn,
		"pics":         []any{},
	}
}

func (f *FakeAfdian) handleMotionList(w http.ResponseWriter, publishSn string) {
	start := 0
	if publishSn != "" {
		for i, p := range f.Posts {
			if p.Sn == publishSn {
				start = i + 1
				break
			}
		}
	}
	end := start + 10
	if end > len(f.Posts) {
		end = len(f.Posts)
	}
	list := []any{}
	for _, p := range f.Posts[start:end] {
		list = append(list, postListItem(p))
	}
	writeJSON(w, map[string]any{"data": map[string]any{"list": list}})
}

func (f *FakeAfdian) handlePostDetail(w http.ResponseWriter, postID string) {
	if f.FailDetail[postID] {
		w.WriteHeader(http.StatusInternalServerError)
		return
	}
	p := f.findPost(postID)
	if p == nil {
		w.WriteHeader(http.StatusNotFound)
		return
	}
	writeJSON(w, map[string]any{"data": map[string]any{"post": map[string]any{
		"content": p.Content,
		"audio":   "",
		"video":   "",
	}}})
}

func (f *FakeAfdian) handleAlbumList(w http.ResponseWriter) {
	list := []any{}
	for _, a := range f.Albums {
		list = append(list, map[string]any{"album_id": a.ID, "title": a.Title})
	}
	writeJSON(w, map[string]any{"data": map[string]any{"list": list}})
}

func (f *FakeAfdian) handleAlbumInfo(w http.ResponseWriter, albumID string) {
	for _, a := range f.Albums {
		if a.ID == albumID {
			writeJSON(w, map[string]any{"data": map[string]any{"album": map[string]any{
				"user":       map[string]any{"url_slug": f.UrlSlug},
				"title":      a.Title,
				"post_count": len(a.PostIDs),
			}}})
			return
		}
	}
	w.WriteHeader(http.StatusNotFound)
}

func (f *FakeAfdian) handleAlbumPost(w http.ResponseWriter, albumID string, lastRank string) {
	for _, a := range f.Albums {
		if a.ID != albumID {
			continue
		}
		start, _ := strconv.Atoi(lastRank)
		end := start + 10
		if end > len(a.PostIDs) {
			end = len(a.PostIDs)
		}
		if start > len(a.PostIDs) {
			start = len(a.PostIDs)
		}
		list := []any{}
		for _, id := range a.PostIDs[start:end] {
			if p := f.findPost(id); p != nil {
				list = append(list, postListItem(*p))
			}
		}
		writeJSON(w, map[string]any{"data": map[string]any{"list": list}})
		return
	}
	w.WriteHeader(http.StatusNotFound)
}

func (f *FakeAfdian) handleProducts(w http.ResponseWriter, page string) {
	const perPage = 10
	pageNum, _ := strconv.Atoi(page)
	if pageNum < 1 {
		pageNum = 1
	}
	start := (pageNum - 1) * perPage
	end := start + perPage
	if start > len(f.Products) {
		start = len(f.Products)
	}
	if end > len(f.Products) {
		end = len(f.Products)
	}
	list := []any{}
	for _, p := range f.Products[start:end] {
		list = append(list, map[string]any{
			"plan_id":     p.ID,
			"name":        p.Name,
			"desc":        p.Desc,
			"price":       p.Price,
			"pic":         "",
			"update_time": p.UpdateTime,
		})
	}
	hasMore := 0
	if end < len(f.Products) {
		hasMore = 1
	}
	writeJSON(w, map[string]any{"data": map[string]any{"list": list, "has_more": hasMore}})
}

// MakePosts 生成 n 篇假动态（新的在前），最旧一篇 Sn 为空表示分页结束
func MakePosts(n int, contentPrefix string) []FakePost {
	posts := make([]FakePost, 0, n)
	for i := 0; i < n; i++ {
		sn := fmt.Sprintf("sn-%d", i)
		if i == n-1 {
			sn = ""
		}
		posts = append(posts, FakePost{
			ID:          fmt.Sprintf("post-%03d", i),
			Title:       fmt.Sprintf("标题%d", i),
			Content:     fmt.Sprintf("<p>%s-%d</p>", contentPrefix, i),
			Sn:          sn,
			PublishTime: 1700000000 + int64((n-i)*3600),
		})
	}
	return posts
}
