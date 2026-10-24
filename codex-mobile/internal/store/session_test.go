package store

import (
	"path/filepath"
	"testing"
)

// 「不指定项目」建的会话不能被塞回上一条对话的目录 —— 手机上
// "新增对话还是进了当前项目"那个 bug 就出在这里。
func TestNewSessionNoProjectKeepsDirEmpty(t *testing.T) {
	db, err := Open(filepath.Join(t.TempDir(), "sessions.json"))
	if err != nil {
		t.Fatal(err)
	}
	project := t.TempDir()
	db.DefaultCwd = t.TempDir()

	first := db.NewSession(project, "")
	if first.Cwd != project {
		t.Fatalf("第一条会话的目录 = %q, 想要 %q", first.Cwd, project)
	}

	noProject := db.NewSessionNoProject("")
	if noProject.Cwd != "" {
		t.Fatalf("「不指定项目」的会话不该绑定目录，实际 = %q", noProject.Cwd)
	}
	if db.LastCwd != project {
		t.Fatalf("LastCwd 被改成了 %q, 应该还是 %q", db.LastCwd, project)
	}

	// 没指定目录（老客户端整段不带 cwd）照旧沿用上次用过的目录
	next := db.NewSession("", "")
	if next.Cwd != project {
		t.Fatalf("没指定目录时 = %q, 想沿用 %q", next.Cwd, project)
	}
}

// 没有"上次用过的目录"时才退回默认目录。
func TestNewSessionFallsBackToDefaultCwd(t *testing.T) {
	db, err := Open(filepath.Join(t.TempDir(), "sessions.json"))
	if err != nil {
		t.Fatal(err)
	}
	db.DefaultCwd = t.TempDir()
	if s := db.NewSession("", ""); s.Cwd != db.DefaultCwd {
		t.Fatalf("没有可沿用的目录时 = %q, 想要默认目录 %q", s.Cwd, db.DefaultCwd)
	}
}

// 切到一条"不指定项目"的对话，不该把"上次用过的目录"抹掉。
func TestSelectNoProjectKeepsLastCwd(t *testing.T) {
	db, err := Open(filepath.Join(t.TempDir(), "sessions.json"))
	if err != nil {
		t.Fatal(err)
	}
	project := t.TempDir()
	db.DefaultCwd = t.TempDir()
	db.NewSession(project, "")
	noProject := db.NewSessionNoProject("")

	if !db.Select(noProject.ID) {
		t.Fatal("选中刚建的会话失败")
	}
	if db.LastCwd != project {
		t.Fatalf("切到「不指定项目」后 LastCwd = %q, 应该还是 %q", db.LastCwd, project)
	}
}
