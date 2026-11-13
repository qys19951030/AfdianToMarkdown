package storage

import (
	"AfdianToMarkdown/afdian"
	"AfdianToMarkdown/config"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"time"

	md "github.com/JohannesKaufmann/html-to-markdown"
	"golang.org/x/exp/slog"
)

// PostSaver 账本感知的文章保存器：以账本为准做增量、去重与失败记录
type PostSaver struct {
	Cfg            *config.Config
	Ledger         *Ledger
	AuthorDir      string // 作者目录绝对路径
	Category       string // 类别目录（相对作者目录），如 motions、作品集名
	AuthToken      string
	DisableComment bool
	Converter      *md.Converter
}

// NewPostSaver 创建文章保存器
func NewPostSaver(cfg *config.Config, ledger *Ledger, authorDir, category, authToken string, disableComment bool, converter *md.Converter) *PostSaver {
	return &PostSaver{
		Cfg:            cfg,
		Ledger:         ledger,
		AuthorDir:      authorDir,
		Category:       category,
		AuthToken:      authToken,
		DisableComment: disableComment,
		Converter:      converter,
	}
}

// SavePost 按账本增量保存一篇文章。
// 返回 upToDate=true 表示该文章在本类别下已是最新（未发正文请求），
// 供快速更新模式判断是否停止后续分页。
func (s *PostSaver) SavePost(article afdian.Post) (upToDate bool, err error) {
	id := article.ID
	if id == "" {
		id = idFromURL(article.Url)
	}
	desiredRel := filepath.ToSlash(filepath.Join(s.Category, postFileName(article.Name, article.PublishTime)))
	listFP := s.listFingerprint(article)

	rec := s.Ledger.Record(id)
	if rec != nil && rec.Status == StatusOK {
		unchanged := false
		if listFP != "" {
			if rec.Fingerprint == "" {
				// 老数据重建的记录指纹未知，首次遇到列表数据时采纳，不重复下载
				rec.Fingerprint = listFP
				unchanged = true
			} else if rec.Fingerprint == listFP {
				unchanged = true
			}
		} else {
			// 列表未提供正文，无法比较时按未变处理，避免每次全量重下
			unchanged = true
		}
		if unchanged {
			upToDate, needFetch, err := s.ensureLocalCopy(rec, article, desiredRel)
			if err != nil {
				return false, s.markFailed(rec, article, desiredRel, err)
			}
			if !needFetch {
				return upToDate, nil
			}
			// 落盘文件被手动删除等情况：继续走抓取流程补回
		}
	}

	if err := s.fetchAndWrite(id, article, desiredRel, listFP); err != nil {
		return false, s.markFailed(s.Ledger.UpsertRecord(id), article, desiredRel, err)
	}
	return false, nil
}

// RetryFailed 重试本类别下账本中标记为失败的文章，成功则清除失败标记
func (s *PostSaver) RetryFailed() error {
	for _, rec := range s.Ledger.FailedRecords(s.Category) {
		slog.Info("重试失败文章", "id", rec.ID, "title", rec.Title, "failCount", rec.FailCount)
		article := afdian.Post{
			ID:          rec.ID,
			Name:        rec.Title,
			Url:         rec.URL,
			Pictures:    rec.Pictures,
			PublishTime: time.Unix(rec.PublishTime, 0),
		}
		if _, err := s.SavePost(article); err != nil {
			if err := s.Cfg.HandleErr(err, "重试失败文章出错", "id", rec.ID, "title", rec.Title); err != nil {
				return err
			}
		}
	}
	return nil
}

// listFingerprint 用列表接口返回的正文计算指纹，避免为比对而发正文请求
func (s *PostSaver) listFingerprint(article afdian.Post) string {
	if article.Content == "" {
		return ""
	}
	body, err := s.Converter.ConvertString(article.Content)
	if err != nil {
		return ""
	}
	return Fingerprint(body)
}

// ensureLocalCopy 在正文未变化时保证本类别下有正确文件名的落盘文件。
// 返回 upToDate（本类别文件已存在且无需任何请求）、needFetch（磁盘无副本需重新抓正文）。
func (s *PostSaver) ensureLocalCopy(rec *PostRecord, article afdian.Post, desiredRel string) (upToDate bool, needFetch bool, err error) {
	// 本类别已有记录：文件名一致则无需动作；标题/时间变化则按 canonical id 改名替换，不留两份
	for _, p := range rec.Paths {
		if pathDir(p) != s.Category {
			continue
		}
		oldAbs := filepath.Join(s.AuthorDir, filepath.FromSlash(p))
		if _, statErr := os.Stat(oldAbs); statErr != nil {
			// 记录中的文件已不在磁盘上，移除记录后按需要补抓
			rec.Paths = removePath(rec.Paths, p)
			continue
		}
		if p == desiredRel {
			return true, false, nil
		}
		desiredAbs := filepath.Join(s.AuthorDir, filepath.FromSlash(desiredRel))
		slog.Info("标题或时间变化，按 canonical id 替换旧文件", "id", rec.ID, "old", p, "new", desiredRel)
		if err := os.MkdirAll(filepath.Dir(desiredAbs), 0o755); err != nil {
			return false, false, err
		}
		if err := os.Rename(oldAbs, desiredAbs); err != nil {
			return false, false, err
		}
		rec.Paths = removePath(rec.Paths, p)
		rec.Paths = appendUnique(rec.Paths, desiredRel)
		return true, false, nil
	}
	// 其它类别已有文件：复用内容（替换 Refer），同一篇文章只抓一次正文
	for _, p := range rec.Paths {
		srcAbs := filepath.Join(s.AuthorDir, filepath.FromSlash(p))
		data, readErr := os.ReadFile(srcAbs)
		if readErr != nil {
			continue
		}
		content := replaceRefer(string(data), referOf(article.Url))
		desiredAbs := filepath.Join(s.AuthorDir, filepath.FromSlash(desiredRel))
		if err := os.MkdirAll(filepath.Dir(desiredAbs), 0o755); err != nil {
			return false, false, err
		}
		slog.Info("复用其它类别已抓取的正文", "id", rec.ID, "from", p, "to", desiredRel)
		if err := WriteFileAtomic(desiredAbs, []byte(content)); err != nil {
			return false, false, err
		}
		copyAssets(content, filepath.Join(s.AuthorDir, filepath.FromSlash(pathDir(p))), filepath.Join(s.AuthorDir, filepath.FromSlash(s.Category)))
		rec.Paths = appendUnique(rec.Paths, desiredRel)
		return false, false, nil
	}
	return false, true, nil
}

// fetchAndWrite 抓取正文并落盘，同步更新其它类别的副本，按 canonical id 清理旧文件
func (s *PostSaver) fetchAndWrite(id string, article afdian.Post, desiredRel string, listFP string) error {
	desiredAbs := filepath.Join(s.AuthorDir, filepath.FromSlash(desiredRel))
	if err := os.MkdirAll(filepath.Dir(desiredAbs), 0o755); err != nil {
		return err
	}
	slog.Info("Saving file:", "path", desiredAbs)
	content, err := buildPostMarkdown(s.Cfg, desiredAbs, article, referOf(article.Url), s.AuthToken, s.DisableComment, s.Converter)
	if err != nil {
		return err
	}
	if err := WriteFileAtomic(desiredAbs, []byte(content)); err != nil {
		return err
	}

	rec := s.Ledger.UpsertRecord(id)
	// 正文变化时同步更新其它类别的副本（保留各自 Refer），保证两处内容一致
	for _, p := range rec.Paths {
		if p == desiredRel || pathDir(p) == s.Category {
			continue
		}
		abs := filepath.Join(s.AuthorDir, filepath.FromSlash(p))
		data, readErr := os.ReadFile(abs)
		if readErr != nil {
			continue
		}
		synced := replaceRefer(content, extractRefer(string(data)))
		if err := WriteFileAtomic(abs, []byte(synced)); err != nil {
			slog.Error("同步其它类别副本失败", "path", abs, "err", err)
			continue
		}
		copyAssets(content, filepath.Join(s.AuthorDir, filepath.FromSlash(s.Category)), filepath.Join(s.AuthorDir, filepath.FromSlash(pathDir(p))))
	}
	// 本类别下的旧文件名（标题/时间变化产生的）按 canonical id 删除，不留两份
	kept := make([]string, 0, len(rec.Paths)+1)
	for _, p := range rec.Paths {
		if pathDir(p) == s.Category && p != desiredRel {
			_ = os.Remove(filepath.Join(s.AuthorDir, filepath.FromSlash(p)))
			continue
		}
		kept = append(kept, p)
	}
	rec.Paths = appendUnique(kept, desiredRel)
	rec.Status = StatusOK
	rec.Error = ""
	rec.FailCount = 0
	rec.Fingerprint = listFP
	rec.URL = article.Url
	rec.Title = article.Name
	rec.PublishTime = article.PublishTime.Unix()
	rec.Pictures = article.Pictures
	rec.UpdatedAt = time.Now().Format(time.RFC3339)
	return nil
}

// markFailed 把失败状态与错误摘要记进账本，连续失败多次打出明确日志
func (s *PostSaver) markFailed(rec *PostRecord, article afdian.Post, desiredRel string, err error) error {
	rec.Status = StatusFailed
	rec.Error = truncate(err.Error(), 200)
	rec.FailCount++
	rec.Paths = appendUnique(rec.Paths, desiredRel)
	rec.URL = article.Url
	rec.Title = article.Name
	rec.PublishTime = article.PublishTime.Unix()
	rec.Pictures = article.Pictures
	rec.UpdatedAt = time.Now().Format(time.RFC3339)
	if rec.FailCount >= 3 {
		slog.Warn("同一篇文章已连续失败多次", "id", rec.ID, "title", rec.Title, "failCount", rec.FailCount, "err", rec.Error)
	}
	return err
}

// postFileName 生成落盘文件名：{yyyy-mm-dd_hh_mm_ss}_{标题}.md
func postFileName(title string, publishTime time.Time) string {
	return publishTime.Format("2006-01-02_15_04_05") + "_" + title + ".md"
}

// idFromURL 从文章 URL 中提取 canonical id（最后一段）
func idFromURL(articleUrl string) string {
	parts := strings.Split(strings.TrimRight(articleUrl, "/"), "/")
	if len(parts) == 0 {
		return ""
	}
	return parts[len(parts)-1]
}

// referOf 与 writer 保持一致的 Refer 链接形式
func referOf(articleUrl string) string {
	return strings.Replace(articleUrl, "post", "p", 1)
}

// pathDir 返回相对路径所在的类别目录（正斜杠形式）
func pathDir(relPath string) string {
	return filepath.ToSlash(filepath.Dir(filepath.FromSlash(relPath)))
}

// removePath 从路径列表中移除指定项
func removePath(paths []string, target string) []string {
	out := paths[:0]
	for _, p := range paths {
		if p != target {
			out = append(out, p)
		}
	}
	return out
}

// truncate 截断错误摘要，避免账本无限膨胀
func truncate(s string, maxLen int) string {
	if len(s) <= maxLen {
		return s
	}
	return s[:maxLen] + "..."
}

// referLineRegexp 匹配 markdown 中 Refer 段的链接行
var referLineRegexp = regexp.MustCompile("(?m)(### Refer\\n\\n)\\S+")

// replaceRefer 替换 markdown 中 Refer 段的链接
func replaceRefer(content string, newRefer string) string {
	if newRefer == "" {
		return content
	}
	return referLineRegexp.ReplaceAllString(content, "1"+newRefer)
}

// extractRefer 提取 markdown 中 Refer 段的链接
func extractRefer(content string) string {
	m := referLineRegexp.FindStringSubmatch(content)
	if m == nil {
		return ""
	}
	line := m[0]
	idx := strings.LastIndex(line, "\n")
	return strings.TrimSpace(line[idx+1:])
}

// assetRefRegexp 匹配 markdown 中对本地 .assets 的引用
var assetRefRegexp = regexp.MustCompile(`\.assets/[^)"\s]+`)

// copyAssets 把 markdown 中引用的本地资源文件从源类别目录复制到目标类别目录（尽力而为）
func copyAssets(content string, fromDir string, toDir string) {
	if fromDir == toDir {
		return
	}
	for _, ref := range assetRefRegexp.FindAllString(content, -1) {
		rel := filepath.FromSlash(ref)
		src := filepath.Join(fromDir, rel)
		dst := filepath.Join(toDir, rel)
		data, err := os.ReadFile(src)
		if err != nil {
			continue
		}
		if err := os.MkdirAll(filepath.Dir(dst), 0o755); err != nil {
			continue
		}
		_ = os.WriteFile(dst, data, 0o644)
	}
}
