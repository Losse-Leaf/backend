package backup

import (
	"database/sql"
	"os"
	"path/filepath"
	"testing"
)

// 备份包里的数据库快照必须**已脱敏**。
//
// 背景：原先把 VACUUM 出来的**原始数据库**直接打进 ZIP，
// 而脱敏只作用于 metadata/data.json 那份镜像 ——
// 于是 `site_configs.r2_secret_access_key`（明文列）与主理人口令哈希
// 原样进了备份包。拿到 ZIP = 同时拿到全部私人内容 + R2 桶写权限密钥
// + 可离线爆破的口令哈希。
func TestSanitizeSnapshotDBClearsCredentials(t *testing.T) {
	dir := t.TempDir()
	snapshotPath := filepath.Join(dir, "snapshot.db")

	// 造一个带凭据的 site_configs
	conn, err := sql.Open("sqlite", snapshotPath)
	if err != nil {
		t.Fatalf("打开数据库失败: %v", err)
	}
	_, err = conn.Exec(`CREATE TABLE site_configs (
		id integer primary key,
		curator_name text,
		master_passcode text,
		r2_access_key_id text,
		r2_secret_access_key text,
		r2_password text
	)`)
	if err != nil {
		t.Fatalf("建表失败: %v", err)
	}
	_, err = conn.Exec(`INSERT INTO site_configs
		(id, curator_name, master_passcode, r2_access_key_id, r2_secret_access_key, r2_password)
		VALUES (1, '迟暮', '$2a$10$hashhashhash', 'AKIAEXAMPLE', 'super-secret-key', 'pw')`)
	if err != nil {
		t.Fatalf("插入数据失败: %v", err)
	}
	conn.Close()

	// 脱敏
	if err := sanitizeSnapshotDB(snapshotPath); err != nil {
		t.Fatalf("脱敏失败: %v", err)
	}

	// 校验：凭据字段必须为空，其他字段必须保留
	conn2, err := sql.Open("sqlite", snapshotPath)
	if err != nil {
		t.Fatalf("重新打开失败: %v", err)
	}
	defer conn2.Close()

	var masterPasscode, accessKeyID, secretKey, password, curator string
	row := conn2.QueryRow(`SELECT master_passcode, r2_access_key_id, r2_secret_access_key, r2_password, curator_name FROM site_configs WHERE id = 1`)
	if err := row.Scan(&masterPasscode, &accessKeyID, &secretKey, &password, &curator); err != nil {
		t.Fatalf("读取失败: %v", err)
	}

	if masterPasscode != "" {
		t.Errorf("主理人口令哈希必须清空，实际 %q", masterPasscode)
	}
	if secretKey != "" {
		t.Errorf("R2 密钥必须清空，实际 %q", secretKey)
	}
	if accessKeyID != "" {
		t.Errorf("R2 AccessKeyId 必须清空，实际 %q", accessKeyID)
	}
	if password != "" {
		t.Errorf("R2 口令必须清空，实际 %q", password)
	}
	if curator != "迟暮" {
		t.Errorf("非凭据字段必须保留，实际 %q", curator)
	}
}

// 紧急保底副本只保留最近一份
func TestPruneEmergencyCopiesKeepsOnlyLatest(t *testing.T) {
	dir := t.TempDir()
	dbPath := filepath.Join(dir, "looseleaf.db")
	if err := os.WriteFile(dbPath, []byte("x"), 0o600); err != nil {
		t.Fatalf("写库文件失败: %v", err)
	}

	// 造 3 份「历史」副本
	base := filepath.Base(dbPath)
	for i := 0; i < 3; i++ {
		p := filepath.Join(dir, base+".pre-restore-emergency-1000"+string(rune('0'+i)))
		if err := os.WriteFile(p, []byte("y"), 0o600); err != nil {
			t.Fatalf("写副本失败: %v", err)
		}
	}

	pruneEmergencyCopies(dbPath, 1)

	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatalf("读目录失败: %v", err)
	}
	copies := 0
	for _, e := range entries {
		if len(e.Name()) > len(base) && e.Name()[:len(base)+len(".pre-restore-emergency-")] == base+".pre-restore-emergency-" {
			copies++
		}
	}
	if copies > 1 {
		t.Errorf("紧急保底副本最多保留 1 份，实际 %d 份", copies)
	}
}
