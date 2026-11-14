package motion

import (
	"AfdianToMarkdown/afdian"
	"AfdianToMarkdown/crawltest"
	"AfdianToMarkdown/storage"
	"fmt"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// fastRetry 关闭请求重试，让测试中的失败立即生效
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

// 中途中断后重跑：已采完的分页零重复请求，数据不缺不重
func TestMotions_ResumeFromCursor(t *testing.T) {
	fastRetry(t)
	f := crawltest.NewFakeAfdian()
	defer f.Close()
	f.Posts = crawltest.MakePosts(11, "正文") // 两页：10 + 1
	dataDir := t.TempDir()
	cfg := f.NewConfig(dataDir, false)

	// 第二页请求时失败一次，模拟中断
	f.FailOnce["/api/post/get-list?user_id="+f.UserID+"&type=new&publish_sn=sn-9&per_page=10&group_id=&all=1&is_public=&plan_id=&title=&name="] = true
	err := GetMotions(cfg, f.UrlSlug, "", "", true, false)
	require.Error(t, err, "第二页失败应中断整批")
	assert.Equal(t, 10, countMarkdownFiles(t, filepath.Join(dataDir, f.UrlSlug, "motions")))

	// 账本游标应指向第二页
	ledger := storage.LoadLedger(filepath.Join(dataDir, f.UrlSlug))
	assert.Equal(t, "sn-9", ledger.GetCursor("motions"))

	// 重跑：第一页（publish_sn 为空）不应再请求
	f.ResetCounts()
	require.NoError(t, GetMotions(cfg, f.UrlSlug, "", "", true, false))
	assert.Equal(t, 0, f.RequestCount("publish_sn=&"), "已采完的分页不应重复请求")
	assert.Equal(t, 1, f.RequestCount("publish_sn=sn-9"))
	assert.Equal(t, 11, countMarkdownFiles(t, filepath.Join(dataDir, f.UrlSlug, "motions")))

	// 整批跑完游标应清除
	ledger = storage.LoadLedger(filepath.Join(dataDir, f.UrlSlug))
	assert.Empty(t, ledger.GetCursor("motions"))
}

// 二次运行无新增时正文请求为 0；新增只抓新增；改过正文的被覆盖更新
func TestMotions_IncrementalByFingerprint(t *testing.T) {
	fastRetry(t)
	f := crawltest.NewFakeAfdian()
	defer f.Close()
	f.Posts = crawltest.MakePosts(3, "正文")
	dataDir := t.TempDir()
	cfg := f.NewConfig(dataDir, false)

	require.NoError(t, GetMotions(cfg, f.UrlSlug, "", "", true, false))
	assert.Equal(t, 3, f.DetailCount())

	// 二次运行：无新增，正文请求为 0
	f.ResetCounts()
	require.NoError(t, GetMotions(cfg, f.UrlSlug, "", "", true, false))
	assert.Equal(t, 0, f.DetailCount(), "无变化时不应发正文请求")

	// 新增一篇：只抓新增
	f.Posts = append([]crawltest.FakePost{{
		ID: "post-new", Title: "新文章", Content: "<p>全新</p>", Sn: "sn-new",
		PublishTime: 1700100000,
	}}, f.Posts...)
	f.ResetCounts()
	require.NoError(t, GetMotions(cfg, f.UrlSlug, "", "", true, false))
	assert.Equal(t, 1, f.DetailCount(), "新增一篇只应抓一篇")
	assert.Equal(t, 4, countMarkdownFiles(t, filepath.Join(dataDir, f.UrlSlug, "motions")))

	// 修改正文：被覆盖更新
	f.Posts[1].Content = "<p>改过的正文</p>"
	f.ResetCounts()
	require.NoError(t, GetMotions(cfg, f.UrlSlug, "", "", true, false))
	assert.Equal(t, 1, f.DetailCount(), "只有改过正文的一篇应重新抓")
	timePrefix := time.Unix(f.Posts[1].PublishTime, 0).Format("2006-01-02_15_04_05")
	data, err := os.ReadFile(filepath.Join(dataDir, f.UrlSlug, "motions", timePrefix+"_标题0.md"))
	require.NoError(t, err)
	assert.Contains(t, string(data), "改过的正文")
}

// 作者改标题或改发布时间：按 canonical id 替换旧文件，不留两份，不发正文请求
func TestMotions_TitleChangeRenames(t *testing.T) {
	fastRetry(t)
	f := crawltest.NewFakeAfdian()
	defer f.Close()
	f.Posts = crawltest.MakePosts(1, "正文")
	dataDir := t.TempDir()
	cfg := f.NewConfig(dataDir, false)

	require.NoError(t, GetMotions(cfg, f.UrlSlug, "", "", true, false))
	motionsDir := filepath.Join(dataDir, f.UrlSlug, "motions")
	assert.Equal(t, 1, countMarkdownFiles(t, motionsDir))

	f.Posts[0].Title = "改过标题"
	f.Posts[0].PublishTime += 3600
	f.ResetCounts()
	require.NoError(t, GetMotions(cfg, f.UrlSlug, "", "", true, false))
	assert.Equal(t, 0, f.DetailCount(), "只改标题不应发正文请求")
	assert.Equal(t, 1, countMarkdownFiles(t, motionsDir), "同一 canonical id 只应有一份文件")
	entries, _ := os.ReadDir(motionsDir)
	assert.Contains(t, entries[0].Name(), "改过标题")
}

// 单篇失败整批继续，失败项进账本，下次运行重试成功并清掉标记
func TestMotions_FailureIsolationAndRetry(t *testing.T) {
	fastRetry(t)
	f := crawltest.NewFakeAfdian()
	defer f.Close()
	f.Posts = crawltest.MakePosts(3, "正文")
	dataDir := t.TempDir()
	cfg := f.NewConfig(dataDir, true) // skip_failed

	f.FailDetail["post-001"] = true
	require.NoError(t, GetMotions(cfg, f.UrlSlug, "", "", true, false), "单篇失败不应终止整批")
	assert.Equal(t, 2, countMarkdownFiles(t, filepath.Join(dataDir, f.UrlSlug, "motions")))

	// 失败项连错误摘要进账本
	ledger := storage.LoadLedger(filepath.Join(dataDir, f.UrlSlug))
	rec := ledger.Record("post-001")
	require.NotNil(t, rec)
	assert.Equal(t, storage.StatusFailed, rec.Status)
	assert.NotEmpty(t, rec.Error)
	assert.Equal(t, 1, rec.FailCount)

	// 恢复后重跑：先重试失败项，成功清标记
	delete(f.FailDetail, "post-001")
	require.NoError(t, GetMotions(cfg, f.UrlSlug, "", "", true, false))
	assert.Equal(t, 3, countMarkdownFiles(t, filepath.Join(dataDir, f.UrlSlug, "motions")))
	ledger = storage.LoadLedger(filepath.Join(dataDir, f.UrlSlug))
	rec = ledger.Record("post-001")
	require.NotNil(t, rec)
	assert.Equal(t, storage.StatusOK, rec.Status)
	assert.Empty(t, rec.Error)
	assert.Equal(t, 0, rec.FailCount)
}

// 账本被截断或非法 JSON 时进程不崩，能重建并继续
func TestMotions_CorruptLedgerRebuilds(t *testing.T) {
	fastRetry(t)
	f := crawltest.NewFakeAfdian()
	defer f.Close()
	f.Posts = crawltest.MakePosts(2, "正文")
	dataDir := t.TempDir()
	cfg := f.NewConfig(dataDir, false)

	require.NoError(t, GetMotions(cfg, f.UrlSlug, "", "", true, false))

	// 截断账本
	authorDir := filepath.Join(dataDir, f.UrlSlug)
	require.NoError(t, os.WriteFile(storage.LedgerPath(authorDir), []byte(`{"version":1,"cursors`), 0o644))
	f.ResetCounts()
	require.NoError(t, GetMotions(cfg, f.UrlSlug, "", "", true, false), "账本截断不应崩溃")
	assert.Equal(t, 0, f.DetailCount(), "重建后不应重复下载已有文章")

	// 非法 JSON
	require.NoError(t, os.WriteFile(storage.LedgerPath(authorDir), []byte("not json"), 0o644))
	require.NoError(t, GetMotions(cfg, f.UrlSlug, "", "", true, false), "非法账本不应崩溃")
	assert.Equal(t, 0, f.DetailCount())
}

// 只有 markdown 没有账本的老 data 目录，首次运行不重复下载已有文章
func TestMotions_LegacyDataNoRedownload(t *testing.T) {
	fastRetry(t)
	f := crawltest.NewFakeAfdian()
	defer f.Close()
	f.Posts = crawltest.MakePosts(2, "正文")
	dataDir := t.TempDir()
	cfg := f.NewConfig(dataDir, false)

	// 手工造老数据：只有 markdown，没有账本
	motionsDir := filepath.Join(dataDir, f.UrlSlug, "motions")
	require.NoError(t, os.MkdirAll(motionsDir, 0o755))
	for i, p := range f.Posts {
		timePrefix := time.Unix(p.PublishTime, 0).Format("2006-01-02_15_04_05")
		content := fmt.Sprintf("## %s\n\n### Refer\n\n%s/p/%s\n\n### 正文\n\n旧正文%d\n", p.Title, f.URL(), p.ID, i)
		require.NoError(t, os.WriteFile(filepath.Join(motionsDir, timePrefix+"_"+p.Title+".md"), []byte(content), 0o644))
	}

	require.NoError(t, GetMotions(cfg, f.UrlSlug, "", "", true, false))
	assert.Equal(t, 0, f.DetailCount(), "老 data 目录首次运行不应重复下载")
	assert.Equal(t, 2, countMarkdownFiles(t, motionsDir))
}
