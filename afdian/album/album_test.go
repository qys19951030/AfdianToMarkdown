package album

import (
	"AfdianToMarkdown/afdian"
	"AfdianToMarkdown/afdian/motion"
	"AfdianToMarkdown/crawltest"
	"AfdianToMarkdown/storage"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func fastRetry(t *testing.T) {
	t.Helper()
	oldAttempts, oldDelay := afdian.RetryAttempts, afdian.RetryDelay
	afdian.RetryAttempts, afdian.RetryDelay = 1, time.Millisecond
	t.Cleanup(func() { afdian.RetryAttempts, afdian.RetryDelay = oldAttempts, oldDelay })
}

func countMarkdownFiles(t *testing.T, dir string) int {
	t.Helper()
	entries, err := os.ReadDir(dir)
	require.NoError(t, err)
	n := 0
	for _, e := range entries {
		if !e.IsDir() && filepath.Ext(e.Name()) == ".md" {
			n++
		}
	}
	return n
}

// 同一篇文章在动态和作品集都出现：正文只抓一次，两处内容一致
func TestAlbum_DedupWithMotions(t *testing.T) {
	fastRetry(t)
	f := crawltest.NewFakeAfdian()
	defer f.Close()
	f.Posts = crawltest.MakePosts(2, "正文")
	f.Albums = []crawltest.FakeAlbum{{ID: "album-1", Title: "作品集甲", PostIDs: []string{"post-000", "post-001"}}}
	dataDir := t.TempDir()
	cfg := f.NewConfig(dataDir, false)

	require.NoError(t, motion.GetMotions(cfg, f.UrlSlug, "", "", true, false))
	f.ResetCounts()
	require.NoError(t, GetAlbums(cfg, f.UrlSlug, "", "", true, false))
	assert.Equal(t, 0, f.DetailCount(), "作品集里的同一篇文章不应再发正文请求")

	// 两处内容一致（除 Refer 行）
	readBody := func(dir string) string {
		entries, err := os.ReadDir(dir)
		require.NoError(t, err)
		for _, e := range entries {
			if strings.HasSuffix(e.Name(), "标题0.md") {
				data, err := os.ReadFile(filepath.Join(dir, e.Name()))
				require.NoError(t, err)
				return string(data)
			}
		}
		t.Fatal("找不到标题0的文件")
		return ""
	}
	motionBody := readBody(filepath.Join(dataDir, f.UrlSlug, "motions"))
	albumBody := readBody(filepath.Join(dataDir, f.UrlSlug, "作品集甲"))
	assert.Contains(t, albumBody, "正文-0")
	// 正文段一致
	bodyOf := func(s string) string {
		idx := strings.Index(s, "### 正文")
		require.GreaterOrEqual(t, idx, 0)
		return s[idx:]
	}
	assert.Equal(t, bodyOf(motionBody), bodyOf(albumBody))
	// Refer 各自保持类别形态
	assert.Contains(t, motionBody, "/p/post-000")
	assert.Contains(t, albumBody, "/album/album-1/post-000")

	// 标题变化后：每个类别仍只有一份文件
	f.Posts[0].Title = "改名标题"
	require.NoError(t, motion.GetMotions(cfg, f.UrlSlug, "", "", true, false))
	require.NoError(t, GetAlbums(cfg, f.UrlSlug, "", "", true, false))
	assert.Equal(t, 2, countMarkdownFiles(t, filepath.Join(dataDir, f.UrlSlug, "motions")))
	assert.Equal(t, 2, countMarkdownFiles(t, filepath.Join(dataDir, f.UrlSlug, "作品集甲")))
}

// 作品集中断后按分页位次续跑
func TestAlbum_ResumeFromCursor(t *testing.T) {
	fastRetry(t)
	f := crawltest.NewFakeAfdian()
	defer f.Close()
	f.Posts = crawltest.MakePosts(11, "正文")
	ids := make([]string, 0, 11)
	for _, p := range f.Posts {
		ids = append(ids, p.ID)
	}
	f.Albums = []crawltest.FakeAlbum{{ID: "album-1", Title: "作品集甲", PostIDs: ids}}
	dataDir := t.TempDir()
	cfg := f.NewConfig(dataDir, false)

	// 第二页（lastRank=10）失败一次
	f.FailOnce["/api/user/get-album-post?album_id=album-1&lastRank=10&rankOrder=desc&rankField=rank"] = true
	err := GetAlbums(cfg, f.UrlSlug, "", "", true, false)
	require.Error(t, err)
	assert.Equal(t, 10, countMarkdownFiles(t, filepath.Join(dataDir, f.UrlSlug, "作品集甲")))

	ledger := storage.LoadLedger(filepath.Join(dataDir, f.UrlSlug))
	assert.Equal(t, "10", ledger.GetCursor("album:album-1"))

	f.ResetCounts()
	require.NoError(t, GetAlbums(cfg, f.UrlSlug, "", "", true, false))
	assert.Equal(t, 0, f.RequestCount("lastRank=0&"), "已采完的分页不应重复请求")
	assert.Equal(t, 1, f.RequestCount("lastRank=10&"))
	assert.Equal(t, 11, countMarkdownFiles(t, filepath.Join(dataDir, f.UrlSlug, "作品集甲")))

	ledger = storage.LoadLedger(filepath.Join(dataDir, f.UrlSlug))
	assert.Empty(t, ledger.GetCursor("album:album-1"))
}
