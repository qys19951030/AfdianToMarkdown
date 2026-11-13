package album

import (
	"AfdianToMarkdown/afdian"
	"AfdianToMarkdown/config"
	"AfdianToMarkdown/storage"
	"fmt"
	"net/url"
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"time"

	md "github.com/JohannesKaufmann/html-to-markdown"
	"golang.org/x/exp/slog"
)

func GetAlbums(cfg *config.Config, authorUrlSlug string, cookieString string, authToken string, disableComment bool, quickUpdate bool) error {
	albumHost, _ := url.JoinPath(cfg.HostUrl, "a", authorUrlSlug, "album")
	slog.Info("album列表页:", "albumHostUrl", albumHost)
	userId, err := afdian.GetAuthorId(cfg, authorUrlSlug, albumHost, cookieString)
	if err != nil {
		return err
	}
	albumList, err := afdian.GetAlbumList(cfg, userId, albumHost, cookieString)
	if err != nil {
		return err
	}
	converter := md.NewConverter("", true, nil)
	for _, album := range albumList {
		slog.Info("Find album: ", "albumName", album.AlbumName)
		err := GetAlbum(cfg, cookieString, authToken, album, disableComment, quickUpdate, converter)
		if err := cfg.HandleErr(err, "下载作品集失败", "album", album.AlbumName, "url", album.AlbumUrl); err != nil {
			return err
		}
	}
	return nil
}

func GetAlbum(cfg *config.Config, cookieString string, authToken string, album afdian.Album, disableComment bool, quickUpdate bool, converter *md.Converter) error {
	//获取作品集的所有文章
	//album.AlbumUrl会类似于 https://afdian.com/album/xyz
	re := regexp.MustCompile("^.*/album/")
	albumId := re.ReplaceAllString(album.AlbumUrl, "")

	albumInfo, err := afdian.GetAlbumInfo(cfg, albumId, cookieString)
	if err != nil {
		return err
	}
	time.Sleep(time.Millisecond * time.Duration(afdian.DelayMs))

	albumSaveDir := filepath.Join(cfg.DataDir, albumInfo.AuthorUrlSlug, albumInfo.AlbumName)
	if err := os.MkdirAll(albumSaveDir, os.ModePerm); err != nil {
		return fmt.Errorf("create album dir <%s> error: %v", albumSaveDir, err)
	}

	// 加载账本：游标续跑、指纹增量、失败重试都以账本为准
	authorAbsDir := filepath.Join(cfg.DataDir, albumInfo.AuthorUrlSlug)
	ledger := storage.LoadLedger(authorAbsDir)
	saveLedger := func() {
		if err := ledger.Save(); err != nil {
			slog.Error("账本保存失败", "err", err)
		}
	}
	saver := storage.NewPostSaver(cfg, ledger, authorAbsDir, albumInfo.AlbumName, authToken, disableComment, converter)

	//先重试上次失败的文章
	if err := saver.RetryFailed(); err != nil {
		saveLedger()
		return err
	}

	cursorKey := "album:" + albumId
	var startRank int64
	if c := ledger.GetCursor(cursorKey); c != "" {
		startRank, _ = strconv.ParseInt(c, 10, 64)
		slog.Info("从游标续跑作品集分页", "album", albumInfo.AlbumName, "lastRank", startRank)
	}

	//边获取边下载
	var i int64
	for i = startRank; i < albumInfo.PostCount; i += 10 {
		postList, err := afdian.GetAlbumPostPage(cfg, albumId, cookieString, i, "desc")
		if err != nil {
			saveLedger()
			return err
		}

		for _, post := range postList {
			upToDate, err := saver.SavePost(post)
			if err := cfg.HandleErr(err, "下载作品集文章失败", "title", post.Name, "url", post.Url); err != nil {
				saveLedger()
				return err
			}
			if quickUpdate && upToDate {
				slog.Info("Quick update: 检测到已采集文章，跳过剩余作品集文章", "album", albumInfo.AlbumName)
				ledger.ClearCursor(cursorKey)
				saveLedger()
				return nil
			}
		}
		//记录已确认采完的分页游标，中断后从这里续跑
		ledger.SetCursor(cursorKey, strconv.FormatInt(i+10, 10))
		saveLedger()
		time.Sleep(time.Millisecond * time.Duration(afdian.DelayMs))
	}
	//整批跑完清掉游标，避免下次误判成没采完
	ledger.ClearCursor(cursorKey)
	saveLedger()
	return nil
}
