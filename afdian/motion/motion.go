package motion

import (
	"AfdianToMarkdown/afdian"
	"AfdianToMarkdown/config"
	"AfdianToMarkdown/storage"
	"fmt"
	"net/url"
	"os"
	"path/filepath"
	"time"

	md "github.com/JohannesKaufmann/html-to-markdown"
	"golang.org/x/exp/slog"
)

const (
	authorDir = "motions"
)

// GetMotions 获取作者的所有作品
func GetMotions(cfg *config.Config, authorUrlSlug string, cookieString string, authToken string, disableComment bool, quickUpdate bool) error {
	authorHost, _ := url.JoinPath(cfg.HostUrl, "a", authorUrlSlug)
	//创建作者文件夹
	authorAbsDir := filepath.Join(cfg.DataDir, authorUrlSlug)
	if err := os.MkdirAll(filepath.Join(authorAbsDir, authorDir), os.ModePerm); err != nil {
		return fmt.Errorf("create author dir error: %v", err)
	}
	slog.Info("作者主页", "authorHostUrl", authorHost)

	// 加载账本：游标续跑、指纹增量、失败重试都以账本为准
	ledger := storage.LoadLedger(authorAbsDir)
	saveLedger := func() {
		if err := ledger.Save(); err != nil {
			slog.Error("账本保存失败", "err", err)
		}
	}

	//获取作者作品列表，边获取边下载
	converter := md.NewConverter("", true, nil)
	saver := storage.NewPostSaver(cfg, ledger, authorAbsDir, authorDir, authToken, disableComment, converter)

	//先重试上次失败的文章
	if err := saver.RetryFailed(); err != nil {
		saveLedger()
		return err
	}

	const cursorKey = "motions"
	prevPublishSn := ledger.GetCursor(cursorKey)
	if prevPublishSn != "" {
		slog.Info("从游标续跑动态分页", "author", authorUrlSlug, "publish_sn", prevPublishSn)
	}
	totalCount := 0
	for {
		subArticleList, publishSn, err := afdian.GetMotionUrlList(cfg, authorUrlSlug, cookieString, prevPublishSn)
		if err != nil {
			saveLedger()
			return err
		}

		for _, article := range subArticleList {
			upToDate, err := saver.SavePost(article)
			if err := cfg.HandleErr(err, "下载动态失败", "title", article.Name, "url", article.Url); err != nil {
				saveLedger()
				return err
			}
			if quickUpdate && upToDate {
				slog.Info("Quick update: 检测到已采集文章，跳过剩余动态", "author", authorUrlSlug)
				ledger.ClearCursor(cursorKey)
				saveLedger()
				return nil
			}
		}

		totalCount += len(subArticleList)
		prevPublishSn = publishSn
		if publishSn == "" {
			break
		}
		//记录已确认采完的分页游标，中断后从这里续跑
		ledger.SetCursor(cursorKey, publishSn)
		saveLedger()
		time.Sleep(time.Millisecond * time.Duration(30))
	}
	//整批跑完清掉游标，避免下次误判成没采完
	ledger.ClearCursor(cursorKey)
	saveLedger()
	slog.Info("postList length:", "count", totalCount)
	return nil
}
