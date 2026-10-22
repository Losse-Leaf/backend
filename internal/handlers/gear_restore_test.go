package handlers

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"loose-leaf/internal/db"
	"loose-leaf/internal/models"

	"github.com/gin-gonic/gin"
)

// 随身设备删除后的恢复（⌘Z 撤销的后端前提）
//
// 背景：设备本体存在**档案的 JSON 列**里，删除是「从数组里移除」——
// 没有 deleted_at 可标记，也就没有恢复的落点；
// 而伴生散页被**软删**后，此前**没有任何恢复端点**。
// 于是误删一件设备，它的照片与手记就永久回不来了。
//
// 现在恢复做两件事：设备按原 id 插回数组、伴生散页取消软删除。
func TestRestoreGearAsset(t *testing.T) {
	gin.SetMode(gin.TestMode)
	setupTestFullDB(t)
	// 设备存在**主理人档案**里，必须先有档案
	if err := db.DB.Create(&models.CuratorProfile{Name: "测试主理人"}).Error; err != nil {
		t.Fatalf("create profile: %v", err)
	}

	r := gin.New()
	write := r.Group("/api/v1")
	{
		write.POST("/assets/gear", AddGearAsset)
		write.DELETE("/assets/gear/:id", DeleteGearAsset)
		write.POST("/assets/gear/restore", RestoreGearAsset)
	}

	// 1. 造一件设备
	gearBody, _ := json.Marshal(map[string]interface{}{
		"name":        "测试相机",
		"category":    "数码器材",
		"spec":        "规格",
		"price_cents": 199900,
	})
	req := httptest.NewRequest(http.MethodPost, "/api/v1/assets/gear", bytes.NewReader(gearBody))
	req.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()
	r.ServeHTTP(w, req)
	if w.Code != http.StatusOK {
		t.Fatalf("录入设备失败: %d %s", w.Code, w.Body.String())
	}
	var addResp Response
	_ = json.Unmarshal(w.Body.Bytes(), &addResp)
	addMap := addResp.Data.(map[string]interface{})
	gearID, _ := addMap["id"].(string)
	if gearID == "" {
		t.Fatalf("录入设备未返回 id: %s", w.Body.String())
	}

	// 2. 造一条挂在它身上的伴生散页
	entry := models.LifeEntry{
		Type:       "life",
		Title:      "相纸里的风景",
		Content:    "正文",
		OccurredAt: time.Now(),
		GearID:     &gearID,
	}
	if err := db.DB.Create(&entry).Error; err != nil {
		t.Fatalf("create entry: %v", err)
	}

	// 3. 删除设备（伴生散页应被软删）
	req = httptest.NewRequest(http.MethodDelete, "/api/v1/assets/gear/"+gearID, nil)
	w = httptest.NewRecorder()
	r.ServeHTTP(w, req)
	if w.Code != http.StatusOK {
		t.Fatalf("删除设备失败: %d %s", w.Code, w.Body.String())
	}

	// 关键：必须用 Unscoped 查 —— GORM 的默认查询**不返回**软删行，
	// 所以「查不到」恰恰说明它是软删态（而不是被物理删了）。
	var afterDelete models.LifeEntry
	if err := db.DB.Unscoped().First(&afterDelete, entry.ID).Error; err != nil {
		t.Fatalf("伴生散页不应被物理删除: %v", err)
	}
	if afterDelete.DeletedAt.Valid == false {
		t.Errorf("伴生散页在删除设备后应处于软删除态（这样才恢复得回来）")
	}

	// 4. 恢复设备（连同伴生散页）
	restoreBody, _ := json.Marshal(map[string]interface{}{
		"id":          gearID,
		"name":        "测试相机",
		"category":    "数码器材",
		"spec":        "规格",
		"price_cents": 199900,
		"index":       0,
	})
	req = httptest.NewRequest(http.MethodPost, "/api/v1/assets/gear/restore", bytes.NewReader(restoreBody))
	req.Header.Set("Content-Type", "application/json")
	w = httptest.NewRecorder()
	r.ServeHTTP(w, req)
	if w.Code != http.StatusOK {
		t.Fatalf("恢复设备失败: %d %s", w.Code, w.Body.String())
	}

	// 4.1 设备回到档案里
	var profile models.CuratorProfile
	if err := db.DB.First(&profile).Error; err != nil {
		t.Fatalf("load profile: %v", err)
	}
	found := false
	for _, g := range loadProfileGear(&profile) {
		if g.ID == gearID {
			found = true
			if g.Name != "测试相机" {
				t.Errorf("恢复后的设备名不对: %s", g.Name)
			}
		}
	}
	if !found {
		t.Errorf("恢复后设备应当回到档案里")
	}

	// 4.2 伴生散页**取消软删除**（这是关键 —— 否则照片与手记回不来）
	var restored models.LifeEntry
	if err := db.DB.First(&restored, entry.ID).Error; err != nil {
		t.Fatalf("恢复后伴生散页应可读: %v", err)
	}
	if restored.DeletedAt.Valid {
		t.Errorf("恢复设备后伴生散页必须取消软删除（否则照片与手记永久回不来）")
	}
}

// 恢复必须幂等：重复提交不得把设备插成两份
func TestRestoreGearAssetIsIdempotent(t *testing.T) {
	gin.SetMode(gin.TestMode)
	setupTestFullDB(t)
	if err := db.DB.Create(&models.CuratorProfile{Name: "测试主理人"}).Error; err != nil {
		t.Fatalf("create profile: %v", err)
	}

	r := gin.New()
	write := r.Group("/api/v1")
	write.POST("/assets/gear/restore", RestoreGearAsset)

	body, _ := json.Marshal(map[string]interface{}{
		"id":       "gear-dup-1",
		"name":     "重复恢复的设备",
		"category": "数码器材",
	})
	for i := 0; i < 2; i++ {
		req := httptest.NewRequest(http.MethodPost, "/api/v1/assets/gear/restore", bytes.NewReader(body))
		req.Header.Set("Content-Type", "application/json")
		w := httptest.NewRecorder()
		r.ServeHTTP(w, req)
		if w.Code != http.StatusOK {
			t.Fatalf("第 %d 次恢复失败: %d %s", i+1, w.Code, w.Body.String())
		}
	}

	var profile models.CuratorProfile
	if err := db.DB.First(&profile).Error; err != nil {
		t.Fatalf("load profile: %v", err)
	}
	count := 0
	for _, g := range loadProfileGear(&profile) {
		if g.ID == "gear-dup-1" {
			count++
		}
	}
	if count != 1 {
		t.Errorf("重复恢复后设备应当只存在一份，实际 %d 份", count)
	}
}
