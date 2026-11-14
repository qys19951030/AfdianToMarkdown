package shop

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

func fastRetry(t *testing.T) {
	t.Helper()
	oldAttempts, oldDelay := afdian.RetryAttempts, afdian.RetryDelay
	afdian.RetryAttempts, afdian.RetryDelay = 1, time.Millisecond
	t.Cleanup(func() { afdian.RetryAttempts, afdian.RetryDelay = oldAttempts, oldDelay })
}

func makeProducts(n int) []crawltest.FakeProduct {
	products := make([]crawltest.FakeProduct, 0, n)
	for i := 0; i < n; i++ {
		products = append(products, crawltest.FakeProduct{
			ID:         fmt.Sprintf("plan-%03d", i),
			Name:       fmt.Sprintf("商品%d", i),
			Desc:       fmt.Sprintf("<p>描述%d</p>", i),
			Price:      "10.00",
			UpdateTime: 1700000000 + int64(i*3600),
		})
	}
	return products
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

// 电铺按页码续跑：中断后重跑，已采完的分页零重复请求
func TestShop_ResumeFromCursor(t *testing.T) {
	fastRetry(t)
	f := crawltest.NewFakeAfdian()
	defer f.Close()
	f.Products = makeProducts(11) // 两页：10 + 1
	dataDir := t.TempDir()
	cfg := f.NewConfig(dataDir, false)
	shopDir := filepath.Join(dataDir, f.UrlSlug, "shop", "default")

	// 第二页请求时失败一次，模拟中断
	f.FailOnce["/api/creator/get-products?user_id="+f.UserID+"&page=2&tag_id="] = true
	err := GetShopProducts(cfg, f.UrlSlug, "", "", false)
	require.Error(t, err)
	assert.Equal(t, 10, countMarkdownFiles(t, shopDir))

	ledger := storage.LoadLedger(filepath.Join(dataDir, f.UrlSlug))
	assert.Equal(t, "2", ledger.GetCursor("shop:"))

	// 重跑：第一页不应再请求
	f.ResetCounts()
	require.NoError(t, GetShopProducts(cfg, f.UrlSlug, "", "", false))
	assert.Equal(t, 0, f.RequestCount("page=1"), "已采完的分页不应重复请求")
	assert.Equal(t, 1, f.RequestCount("page=2"))
	assert.Equal(t, 11, countMarkdownFiles(t, shopDir))

	ledger = storage.LoadLedger(filepath.Join(dataDir, f.UrlSlug))
	assert.Empty(t, ledger.GetCursor("shop:"), "整批跑完游标应清除")
}

// 二次运行无新增时不改写文件；改描述的商品被覆盖更新；改名的按 canonical id 替换
func TestShop_Incremental(t *testing.T) {
	fastRetry(t)
	f := crawltest.NewFakeAfdian()
	defer f.Close()
	f.Products = makeProducts(2)
	dataDir := t.TempDir()
	cfg := f.NewConfig(dataDir, false)
	shopDir := filepath.Join(dataDir, f.UrlSlug, "shop", "default")

	require.NoError(t, GetShopProducts(cfg, f.UrlSlug, "", "", false))
	assert.Equal(t, 2, countMarkdownFiles(t, shopDir))
	timePrefix := time.Unix(f.Products[0].UpdateTime, 0).Format("2006-01-02_15_04_05")
	firstPath := filepath.Join(shopDir, timePrefix+"_商品0.md")
	info1, err := os.Stat(firstPath)
	require.NoError(t, err)

	// 二次运行：无变化，文件不被改写
	time.Sleep(20 * time.Millisecond)
	require.NoError(t, GetShopProducts(cfg, f.UrlSlug, "", "", false))
	info2, err := os.Stat(firstPath)
	require.NoError(t, err)
	assert.Equal(t, info1.ModTime(), info2.ModTime(), "无变化时不应改写文件")

	// 改描述：覆盖更新
	f.Products[0].Desc = "<p>改过的描述</p>"
	require.NoError(t, GetShopProducts(cfg, f.UrlSlug, "", "", false))
	data, err := os.ReadFile(firstPath)
	require.NoError(t, err)
	assert.Contains(t, string(data), "改过的描述")

	// 改名称：按 canonical id 替换旧文件，不留两份
	f.Products[0].Name = "改名商品"
	require.NoError(t, GetShopProducts(cfg, f.UrlSlug, "", "", false))
	assert.Equal(t, 2, countMarkdownFiles(t, shopDir))
	entries, _ := os.ReadDir(shopDir)
	names := []string{}
	for _, e := range entries {
		names = append(names, e.Name())
	}
	assert.Contains(t, names, timePrefix+"_改名商品.md")
}
