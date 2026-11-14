package storage

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"time"

	"golang.org/x/exp/slog"
)

// LedgerFileName 账本文件名，存放在作者目录下，随 data 一起走
const LedgerFileName = "ledger.json"

// 文章采集状态
const (
	StatusOK     = "ok"
	StatusFailed = "failed"
)

// ledgerVersion 账本格式版本，字段缺失或版本不符时按损坏处理
const ledgerVersion = 1

// PostRecord 账本中一篇文章的记录
type PostRecord struct {
	ID          string   `json:"id"`                    // canonical id（Refer 中的 post hash / 电铺 plan_id）
	Status      string   `json:"status"`                // ok 或 failed
	Error       string   `json:"error,omitempty"`       // 最近一次失败的错误摘要
	FailCount   int      `json:"fail_count,omitempty"`  // 连续失败次数
	Fingerprint string   `json:"fingerprint,omitempty"` // 正文指纹（列表正文 markdown 的 sha256），空表示未知（待采纳）
	Paths       []string `json:"paths"`                 // 落盘路径（相对作者目录），同一 id 可能同时存在于 motions 与作品集
	URL         string   `json:"url"`                   // 文章地址，用于失败重试
	Title       string   `json:"title"`                 // 最近一次见到的标题（安全文件名形式）
	PublishTime int64    `json:"publish_time"`          // 发布时间（unix 秒）
	Pictures    []string `json:"pictures,omitempty"`    // 图片地址，用于失败重试
	UpdatedAt   string   `json:"updated_at"`            // 最近一次采集时间（RFC3339）
}

// ledgerFile 账本磁盘格式
type ledgerFile struct {
	Version int                    `json:"version"`
	Cursors map[string]string      `json:"cursors"` // 各类别分页游标：motions=>publish_sn，album:{id}=>next lastRank，shop:{tag}=>next page
	Posts   map[string]*PostRecord `json:"posts"`   // canonical id => 记录
}

// Ledger 作者级采集账本
type Ledger struct {
	Cursors map[string]string
	Posts   map[string]*PostRecord

	authorDir string
	path      string
}

// LedgerPath 返回作者目录下账本文件的路径
func LedgerPath(authorDir string) string {
	return filepath.Join(authorDir, LedgerFileName)
}

// LoadLedger 加载作者目录下的账本。
// 账本不存在、被截断、不是合法 JSON 或字段缺失时不报错：
// 记录原因后按现有 markdown 文件重建（这批内容按未采过/已落盘状态恢复）。
func LoadLedger(authorDir string) *Ledger {
	path := LedgerPath(authorDir)
	data, err := os.ReadFile(path)
	if err != nil {
		if !os.IsNotExist(err) {
			slog.Warn("账本读取失败，按现有文件重建", "path", path, "err", err)
		} else {
			slog.Info("账本不存在，按现有文件重建", "path", path)
		}
		return rebuildLedger(authorDir)
	}
	var f ledgerFile
	if err := json.Unmarshal(data, &f); err != nil {
		slog.Warn("账本不是合法 JSON（可能被截断），按现有文件重建", "path", path, "err", err)
		return rebuildLedger(authorDir)
	}
	if f.Version != ledgerVersion || f.Cursors == nil || f.Posts == nil {
		slog.Warn("账本字段缺失或版本不符，按现有文件重建", "path", path, "version", f.Version)
		return rebuildLedger(authorDir)
	}
	return &Ledger{Cursors: f.Cursors, Posts: f.Posts, authorDir: authorDir, path: path}
}

// Save 原子写账本：先写临时文件再替换，进程被杀也不会留下半截账本
func (l *Ledger) Save() error {
	if l.path == "" {
		l.path = LedgerPath(l.authorDir)
	}
	f := ledgerFile{Version: ledgerVersion, Cursors: l.Cursors, Posts: l.Posts}
	data, err := json.MarshalIndent(f, "", "  ")
	if err != nil {
		return fmt.Errorf("序列化账本失败: %w", err)
	}
	tmp := l.path + ".tmp"
	if err := os.WriteFile(tmp, data, 0o644); err != nil {
		return fmt.Errorf("写账本临时文件失败: %w", err)
	}
	if err := os.Rename(tmp, l.path); err != nil {
		return fmt.Errorf("替换账本文件失败: %w", err)
	}
	return nil
}

// GetCursor 读取类别游标，不存在返回空串
func (l *Ledger) GetCursor(key string) string {
	return l.Cursors[key]
}

// SetCursor 记录类别游标
func (l *Ledger) SetCursor(key, value string) {
	l.Cursors[key] = value
}

// ClearCursor 整批采完后清除游标，避免下次误判成没采完
func (l *Ledger) ClearCursor(key string) {
	delete(l.Cursors, key)
}

// Record 返回指定 id 的记录，不存在返回 nil
func (l *Ledger) Record(id string) *PostRecord {
	return l.Posts[id]
}

// UpsertRecord 返回指定 id 的记录，不存在则创建
func (l *Ledger) UpsertRecord(id string) *PostRecord {
	if rec, ok := l.Posts[id]; ok {
		return rec
	}
	rec := &PostRecord{ID: id}
	l.Posts[id] = rec
	return rec
}

// FailedRecords 返回指定类别目录下处于失败状态的记录
func (l *Ledger) FailedRecords(category string) []*PostRecord {
	var out []*PostRecord
	prefix := category + string(filepath.Separator)
	for _, rec := range l.Posts {
		if rec.Status != StatusFailed {
			continue
		}
		for _, p := range rec.Paths {
			if strings.HasPrefix(filepath.FromSlash(p), prefix) {
				out = append(out, rec)
				break
			}
		}
	}
	return out
}

// Fingerprint 计算正文指纹（sha256 十六进制）
func Fingerprint(text string) string {
	sum := sha256.Sum256([]byte(strings.TrimSpace(text)))
	return hex.EncodeToString(sum[:])
}

// WriteFileAtomic 原子写文件：先写临时文件再替换，避免进程被杀留下半截文件
func WriteFileAtomic(path string, content []byte) error {
	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, content, 0o644); err != nil {
		return err
	}
	return os.Rename(tmp, path)
}

// referHashRegexp 从 markdown 的 Refer 行提取 canonical id，兼容任意主站域名：
//   - https://afdian.com/p/<hash>
//   - https://afdian.com/album/<albumId>/<hash>
var referHashRegexp = regexp.MustCompile(`/(?:p|album/[a-zA-Z0-9-]+)/([a-zA-Z0-9-]+)`)

// shopLinkRegexp 从电铺 markdown 的链接行提取 plan_id
var shopLinkRegexp = regexp.MustCompile(`/item/([a-zA-Z0-9-]+)`)

// ExtractReferHash 从 markdown 内容中提取 Refer 的 canonical id，缺失返回 ""
func ExtractReferHash(content string) string {
	lines := strings.Split(content, "\n")
	limit := 15
	if len(lines) < limit {
		limit = len(lines)
	}
	for _, line := range lines[:limit] {
		if m := referHashRegexp.FindStringSubmatch(line); m != nil {
			return m[1]
		}
	}
	return ""
}

// rebuildLedger 扫描作者目录下已有的 markdown 文件，重建账本状态。
// 重建出的记录指纹为空（首次遇到列表数据时采纳），状态视为成功。
func rebuildLedger(authorDir string) *Ledger {
	l := &Ledger{
		Cursors:   map[string]string{},
		Posts:     map[string]*PostRecord{},
		authorDir: authorDir,
		path:      LedgerPath(authorDir),
	}
	entries, err := os.ReadDir(authorDir)
	if err != nil {
		return l
	}
	for _, entry := range entries {
		if !entry.IsDir() || entry.Name() == ".assets" {
			continue
		}
		if entry.Name() == "shop" {
			rebuildShopCategory(l, filepath.Join(authorDir, entry.Name()), entry.Name())
			continue
		}
		rebuildPostCategory(l, filepath.Join(authorDir, entry.Name()), entry.Name())
	}
	return l
}

// rebuildPostCategory 扫描动态/作品集目录，按 Refer hash 重建文章记录
func rebuildPostCategory(l *Ledger, dir string, category string) {
	files, err := os.ReadDir(dir)
	if err != nil {
		return
	}
	for _, f := range files {
		if f.IsDir() || !strings.HasSuffix(f.Name(), ".md") {
			continue
		}
		fullPath := filepath.Join(dir, f.Name())
		data, err := os.ReadFile(fullPath)
		if err != nil {
			continue
		}
		id := ExtractReferHash(string(data))
		if id == "" {
			slog.Debug("重建账本：文件缺少 Refer，跳过", "path", fullPath)
			continue
		}
		relPath, err := filepath.Rel(l.authorDir, fullPath)
		if err != nil {
			continue
		}
		rec := l.UpsertRecord(id)
		rec.Status = StatusOK
		rec.Paths = appendUnique(rec.Paths, filepath.ToSlash(relPath))
		if rec.Title == "" {
			rec.Title = titleFromFileName(f.Name())
		}
		if rec.PublishTime == 0 {
			rec.PublishTime = publishTimeFromFileName(f.Name())
		}
	}
}

// rebuildShopCategory 扫描电铺目录，按商品链接中的 plan_id 重建记录
func rebuildShopCategory(l *Ledger, shopDir string, category string) {
	tagDirs, err := os.ReadDir(shopDir)
	if err != nil {
		return
	}
	for _, tagDir := range tagDirs {
		if !tagDir.IsDir() {
			continue
		}
		dir := filepath.Join(shopDir, tagDir.Name())
		files, err := os.ReadDir(dir)
		if err != nil {
			continue
		}
		for _, f := range files {
			if f.IsDir() || !strings.HasSuffix(f.Name(), ".md") {
				continue
			}
			fullPath := filepath.Join(dir, f.Name())
			data, err := os.ReadFile(fullPath)
			if err != nil {
				continue
			}
			id := ""
			if m := shopLinkRegexp.FindStringSubmatch(string(data)); m != nil {
				id = m[1]
			}
			if id == "" {
				continue
			}
			relPath, err := filepath.Rel(l.authorDir, fullPath)
			if err != nil {
				continue
			}
			rec := l.UpsertRecord(id)
			rec.Status = StatusOK
			rec.Paths = appendUnique(rec.Paths, filepath.ToSlash(relPath))
			if rec.Title == "" {
				rec.Title = titleFromFileName(f.Name())
			}
			if rec.PublishTime == 0 {
				rec.PublishTime = publishTimeFromFileName(f.Name())
			}
		}
	}
}

// appendUnique 追加不重复元素
func appendUnique(slice []string, item string) []string {
	for _, s := range slice {
		if s == item {
			return slice
		}
	}
	return append(slice, item)
}

// titleFromFileName 从 {时间戳}_{标题}.md 文件名中提取标题
func titleFromFileName(fileName string) string {
	name := strings.TrimSuffix(fileName, ".md")
	if len(name) > 20 && name[19] == '_' {
		return name[20:]
	}
	return name
}

// publishTimeFromFileName 从文件名前缀解析发布时间（unix 秒），失败返回 0
func publishTimeFromFileName(fileName string) int64 {
	if len(fileName) < 19 || fileName[19] != '_' {
		return 0
	}
	t, err := time.ParseInLocation("2006-01-02_15_04_05", fileName[:19], time.Local)
	if err != nil {
		return 0
	}
	return t.Unix()
}
