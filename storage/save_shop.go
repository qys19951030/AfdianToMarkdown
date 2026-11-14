package storage

import (
	"AfdianToMarkdown/afdian"
	"AfdianToMarkdown/config"
	"AfdianToMarkdown/utils"
	"context"
	"fmt"
	"os"
	"path"
	"path/filepath"
	"time"

	md "github.com/JohannesKaufmann/html-to-markdown"
	"github.com/carlmjohnson/requests"
	"golang.org/x/exp/slog"
)

// ShopSaver 账本感知的电铺商品保存器
type ShopSaver struct {
	Cfg       *config.Config
	Ledger    *Ledger
	AuthorDir string // 作者目录绝对路径
	Category  string // 类别目录（相对作者目录），如 shop/default
	Converter *md.Converter
}

// NewShopSaver 创建电铺商品保存器
func NewShopSaver(cfg *config.Config, ledger *Ledger, authorDir, category string, converter *md.Converter) *ShopSaver {
	return &ShopSaver{Cfg: cfg, Ledger: ledger, AuthorDir: authorDir, Category: category, Converter: converter}
}

// SaveProduct 按账本增量保存一个商品。
// 电铺商品没有单独的正文请求，描述由列表接口直接提供，指纹基于描述 markdown。
// 返回 upToDate=true 表示该商品已是最新（未改写文件）。
func (s *ShopSaver) SaveProduct(product afdian.Product) (upToDate bool, err error) {
	id := product.ID
	desiredRel := filepath.ToSlash(filepath.Join(s.Category, postFileName(product.Name, product.UpdateTime)))

	markdownDesc, convErr := s.Converter.ConvertString(product.Desc)
	if convErr != nil {
		slog.Error("转换 Markdown 失败", "err", convErr, "product", product.Name)
		markdownDesc = product.Desc // 回退到原始 HTML
	}
	fp := Fingerprint(markdownDesc)

	rec := s.Ledger.Record(id)
	if rec != nil && rec.Status == StatusOK {
		if rec.Fingerprint == "" {
			rec.Fingerprint = fp // 老数据重建的记录首次采纳指纹
		}
		if rec.Fingerprint == fp {
			// 描述未变：文件名一致则跳过；名称/时间变化则按 canonical id 改名替换
			for _, p := range append([]string(nil), rec.Paths...) {
				if pathDir(p) != s.Category {
					continue
				}
				oldAbs := filepath.Join(s.AuthorDir, filepath.FromSlash(p))
				if _, statErr := os.Stat(oldAbs); statErr != nil {
					rec.Paths = removePath(rec.Paths, p)
					continue
				}
				if p == desiredRel {
					slog.Info("商品已存在，跳过", "product", product.Name)
					return true, nil
				}
				desiredAbs := filepath.Join(s.AuthorDir, filepath.FromSlash(desiredRel))
				slog.Info("商品名称或时间变化，按 canonical id 替换旧文件", "id", id, "old", p, "new", desiredRel)
				if err := os.MkdirAll(filepath.Dir(desiredAbs), 0o755); err != nil {
					return false, s.markFailed(rec, product, desiredRel, err)
				}
				if err := os.Rename(oldAbs, desiredAbs); err != nil {
					return false, s.markFailed(rec, product, desiredRel, err)
				}
				rec.Paths = removePath(rec.Paths, p)
				rec.Paths = appendUnique(rec.Paths, desiredRel)
				return true, nil
			}
		}
	}

	// 新商品或描述已变化：生成内容并原子落盘
	slog.Info("正在保存商品", "name", product.Name, "price", product.Price)
	desiredAbs := filepath.Join(s.AuthorDir, filepath.FromSlash(desiredRel))
	if err := os.MkdirAll(filepath.Dir(desiredAbs), 0o755); err != nil {
		return false, s.markFailed(s.Ledger.UpsertRecord(id), product, desiredRel, err)
	}
	content := s.buildProductMarkdown(product, markdownDesc, desiredAbs)
	if err := WriteFileAtomic(desiredAbs, []byte(content)); err != nil {
		return false, s.markFailed(s.Ledger.UpsertRecord(id), product, desiredRel, err)
	}

	rec = s.Ledger.UpsertRecord(id)
	// 本类别下的旧文件名按 canonical id 删除，不留两份
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
	rec.Fingerprint = fp
	rec.URL = product.Url
	rec.Title = product.Name
	rec.PublishTime = product.UpdateTime.Unix()
	rec.UpdatedAt = time.Now().Format(time.RFC3339)
	return false, nil
}

// buildProductMarkdown 拼装商品 markdown（含封面图下载，失败回退远程链接）
func (s *ShopSaver) buildProductMarkdown(product afdian.Product, markdownDesc string, filePath string) string {
	picContent := ""
	if product.Pic != "" {
		saveDir := filepath.Dir(filePath)
		assetsDir := filepath.Join(saveDir, utils.ImgDir)
		_ = os.MkdirAll(assetsDir, 0o755)

		ext := filepath.Ext(product.Pic)
		if ext == "" {
			ext = ".jpg"
		}
		imgName := fmt.Sprintf("%s_cover%s", product.ID, ext)
		imgPath := filepath.Join(assetsDir, imgName)

		err := requests.
			URL(product.Pic).
			Header("user-agent", afdian.ChromeUserAgent).
			ToFile(imgPath).
			Fetch(context.Background())

		if err == nil {
			picContent = fmt.Sprintf("![cover](%s)\n\n", path.Join(utils.ImgDir, imgName))
		} else {
			picContent = fmt.Sprintf("![cover](%s)\n\n", product.Pic)
		}
	}

	tagDisplay := product.TagID
	if tagDisplay == "" {
		tagDisplay = "全部"
	}

	return fmt.Sprintf("## %s\n\n**价格**: %s\n\n**标签**: %s\n\n**链接**: %s\n\n**更新时间**: %s\n\n---\n\n%s%s",
		product.Name, product.Price, tagDisplay, product.Url, product.UpdateTime.Format("2006-01-02 15:04:05"), picContent, markdownDesc)
}

// markFailed 把商品失败状态与错误摘要记进账本
func (s *ShopSaver) markFailed(rec *PostRecord, product afdian.Product, desiredRel string, err error) error {
	rec.Status = StatusFailed
	rec.Error = truncate(err.Error(), 200)
	rec.FailCount++
	rec.Paths = appendUnique(rec.Paths, desiredRel)
	rec.URL = product.Url
	rec.Title = product.Name
	rec.PublishTime = product.UpdateTime.Unix()
	rec.UpdatedAt = time.Now().Format(time.RFC3339)
	if rec.FailCount >= 3 {
		slog.Warn("同一商品已连续失败多次", "id", rec.ID, "name", product.Name, "failCount", rec.FailCount, "err", rec.Error)
	}
	return err
}
