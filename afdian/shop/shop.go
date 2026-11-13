package shop

import (
	"AfdianToMarkdown/afdian"
	"AfdianToMarkdown/config"
	"AfdianToMarkdown/storage"
	"fmt"
	"net/url"
	"os"
	"path/filepath"
	"strconv"
	"time"

	md "github.com/JohannesKaufmann/html-to-markdown"
	"golang.org/x/exp/slog"
)

const (
	shopDir = "shop"
)

// GetShopProducts 获取作者电铺的所有商品
func GetShopProducts(cfg *config.Config, authorUrlSlug string, cookieString string, tagId string, quickUpdate bool) error {
	authorHost, _ := url.JoinPath(cfg.HostUrl, "a", authorUrlSlug, "?tab=shop")
	//创建存储文件夹
	subDir := tagId
	if subDir == "" {
		subDir = "default"
	}
	authorAbsDir := filepath.Join(cfg.DataDir, authorUrlSlug)
	saveDir := filepath.Join(authorAbsDir, shopDir, subDir)

	if err := os.MkdirAll(saveDir, os.ModePerm); err != nil {
		return fmt.Errorf("create shop dir error: %v", err)
	}
	slog.Info("开始获取电铺内容", "authorHostUrl", authorHost, "tagId", tagId, "saveDir", saveDir)

	// 加载账本：游标续跑、指纹增量、失败重试都以账本为准
	ledger := storage.LoadLedger(authorAbsDir)
	saveLedger := func() {
		if err := ledger.Save(); err != nil {
			slog.Error("账本保存失败", "err", err)
		}
	}
	category := filepath.ToSlash(filepath.Join(shopDir, subDir))
	converter := md.NewConverter("", true, nil)
	saver := storage.NewShopSaver(cfg, ledger, authorAbsDir, category, converter)

	userId, err := afdian.GetAuthorId(cfg, authorUrlSlug, authorHost, cookieString)
	if err != nil {
		return err
	}

	cursorKey := "shop:" + tagId
	page := 1
	if c := ledger.GetCursor(cursorKey); c != "" {
		if n, err := strconv.Atoi(c); err == nil && n > 1 {
			page = n
			slog.Info("从游标续跑电铺分页", "author", authorUrlSlug, "tagId", tagId, "page", page)
		}
	}
	totalCount := 0
	for {
		products, hasMore, err := afdian.GetProductPage(cfg, userId, cookieString, tagId, page)
		if err != nil {
			saveLedger()
			return err
		}
		if len(products) == 0 {
			break
		}
		for _, product := range products {
			upToDate, err := saver.SaveProduct(product)
			if err := cfg.HandleErr(err, "保存商品失败", "product", product.Name, "url", product.Url); err != nil {
				saveLedger()
				return err
			}
			if quickUpdate && upToDate {
				slog.Info("Quick update: 检测到已采集商品，停止更新电铺", "product", product.Name)
				ledger.ClearCursor(cursorKey)
				saveLedger()
				return nil
			}
			if !upToDate {
				time.Sleep(time.Millisecond * time.Duration(afdian.DelayMs))
			}
		}
		totalCount += len(products)
		page++
		//记录已确认采完的分页游标，中断后从这里续跑
		ledger.SetCursor(cursorKey, strconv.Itoa(page))
		saveLedger()
		if !hasMore {
			break
		}
		time.Sleep(time.Millisecond * time.Duration(afdian.DelayMs))
	}

	//整批跑完清掉游标，避免下次误判成没采完
	ledger.ClearCursor(cursorKey)
	saveLedger()
	slog.Info("电铺内容获取完成", "count", totalCount)
	return nil
}
