package storage

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// writePostFile 在作者目录下生成一份符合四段结构的 markdown
func writePostFile(t *testing.T, authorDir, category, fileName, referURL string) string {
	t.Helper()
	dir := filepath.Join(authorDir, category)
	require.NoError(t, os.MkdirAll(dir, 0o755))
	content := "## 标题\n\n### Refer\n\n" + referURL + "\n\n### 正文\n\n正文内容\n\n## 评论\n\n"
	path := filepath.Join(dir, fileName)
	require.NoError(t, os.WriteFile(path, []byte(content), 0o644))
	return path
}

func TestLedger_SaveLoadRoundTrip(t *testing.T) {
	dir := t.TempDir()
	l := LoadLedger(dir)
	l.SetCursor("motions", "sn123")
	rec := l.UpsertRecord("abc123")
	rec.Status = StatusOK
	rec.Fingerprint = Fingerprint("正文")
	rec.Paths = []string{"motions/2024-01-01_00_00_00_标题.md"}
	require.NoError(t, l.Save())

	// 原子写：临时文件不应残留
	_, err := os.Stat(filepath.Join(dir, LedgerFileName+".tmp"))
	assert.True(t, os.IsNotExist(err), "临时文件应已被替换")

	loaded := LoadLedger(dir)
	assert.Equal(t, "sn123", loaded.GetCursor("motions"))
	got := loaded.Record("abc123")
	require.NotNil(t, got)
	assert.Equal(t, StatusOK, got.Status)
	assert.Equal(t, Fingerprint("正文"), got.Fingerprint)
	assert.Equal(t, []string{"motions/2024-01-01_00_00_00_标题.md"}, got.Paths)
}

func TestLedger_CorruptJSONDoesNotCrash(t *testing.T) {
	dir := t.TempDir()
	// 被截断的账本（进程被杀留下的半截文件）
	require.NoError(t, os.WriteFile(LedgerPath(dir), []byte(`{"version":1,"cursors":{"mot`), 0o644))
	writePostFile(t, dir, "motions", "2024-01-01_00_00_00_旧文.md", "https://afdian.com/p/hash001")

	l := LoadLedger(dir) // 不应 panic，按现有文件重建
	rec := l.Record("hash001")
	require.NotNil(t, rec, "应从现有 markdown 重建出记录")
	assert.Equal(t, StatusOK, rec.Status)
	assert.Equal(t, []string{"motions/2024-01-01_00_00_00_旧文.md"}, rec.Paths)
	assert.Empty(t, rec.Fingerprint, "重建的记录指纹未知，等待采纳")
}

func TestLedger_InvalidJSONRebuilds(t *testing.T) {
	dir := t.TempDir()
	require.NoError(t, os.WriteFile(LedgerPath(dir), []byte("这不是JSON"), 0o644))
	l := LoadLedger(dir)
	assert.NotNil(t, l)
	assert.Empty(t, l.Posts)
}

func TestLedger_MissingFieldsRebuilds(t *testing.T) {
	dir := t.TempDir()
	// 合法 JSON 但字段缺失
	require.NoError(t, os.WriteFile(LedgerPath(dir), []byte(`{"version":1}`), 0o644))
	writePostFile(t, dir, "motions", "2024-01-01_00_00_00_旧文.md", "https://afdian.com/p/hash002")

	l := LoadLedger(dir)
	require.NotNil(t, l.Record("hash002"), "字段缺失应触发重建")
}

func TestLedger_RebuildFromLegacyData(t *testing.T) {
	dir := t.TempDir()
	// 老 data 目录：动态 + 作品集 + 电铺，无账本
	writePostFile(t, dir, "motions", "2024-01-01_10_00_00_文章A.md", "https://afdian.com/p/postA")
	writePostFile(t, dir, "作品集甲", "2024-01-02_10_00_00_文章A.md", "https://afdian.com/album/albumX/postA")
	writePostFile(t, dir, "作品集甲", "2024-01-03_10_00_00_文章B.md", "https://afdian.com/album/albumX/postB")
	shopDir := filepath.Join(dir, "shop", "default")
	require.NoError(t, os.MkdirAll(shopDir, 0o755))
	shopContent := "## 商品\n\n**价格**: 10\n\n**链接**: https://afdian.com/item/plan123\n\n---\n\n描述"
	require.NoError(t, os.WriteFile(filepath.Join(shopDir, "2024-01-04_00_00_00_商品.md"), []byte(shopContent), 0o644))

	l := LoadLedger(dir)

	// 同一篇文章在动态和作品集各一份，合并为一条记录两个路径
	recA := l.Record("postA")
	require.NotNil(t, recA)
	assert.ElementsMatch(t, []string{"motions/2024-01-01_10_00_00_文章A.md", "作品集甲/2024-01-02_10_00_00_文章A.md"}, recA.Paths)

	require.NotNil(t, l.Record("postB"))
	require.NotNil(t, l.Record("plan123"), "电铺商品应按 plan_id 重建")

	// 重建后落盘，下次直接加载
	require.NoError(t, l.Save())
	reloaded := LoadLedger(dir)
	require.NotNil(t, reloaded.Record("postA"))
}

func TestLedger_FailedRecordsByCategory(t *testing.T) {
	dir := t.TempDir()
	l := LoadLedger(dir)
	rec := l.UpsertRecord("fail1")
	rec.Status = StatusFailed
	rec.Error = "boom"
	rec.Paths = []string{"motions/x.md"}
	other := l.UpsertRecord("fail2")
	other.Status = StatusFailed
	other.Paths = []string{"作品集甲/y.md"}

	failed := l.FailedRecords("motions")
	require.Len(t, failed, 1)
	assert.Equal(t, "fail1", failed[0].ID)
}

func TestWriteFileAtomic(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "a.md")
	require.NoError(t, WriteFileAtomic(path, []byte("内容")))
	data, err := os.ReadFile(path)
	require.NoError(t, err)
	assert.Equal(t, "内容", string(data))
	_, err = os.Stat(path + ".tmp")
	assert.True(t, os.IsNotExist(err))
}

func TestExtractReferHash(t *testing.T) {
	assert.Equal(t, "abc123", ExtractReferHash("## t\n\n### Refer\n\nhttps://afdian.com/p/abc123\n"))
	assert.Equal(t, "def456", ExtractReferHash("### Refer\n\nhttps://ifdian.net/album/album1/def456\n"))
	assert.Empty(t, ExtractReferHash("## 没有Refer\n"))
}
