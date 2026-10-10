package handlers

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"loose-leaf/internal/config"
	"loose-leaf/internal/db"
	"loose-leaf/internal/models"

	"github.com/gin-gonic/gin"
	"github.com/glebarez/sqlite"
	"gorm.io/gorm"
)

func setupTestFullDB(t *testing.T) {
	database, err := gorm.Open(sqlite.Open(":memory:"), &gorm.Config{})
	if err != nil {
		t.Fatalf("failed to open test in-memory db: %v", err)
	}
	_ = database.AutoMigrate(
		&models.SiteConfig{},
		&models.CuratorProfile{},
		&models.Device{},
		&models.PairTicket{},
		&models.QRSession{},
		&models.Attachment{},
		&models.LifeEntry{},
		&models.Transaction{},
		&models.Project{},
		&models.Wish{},
		&models.Footprint{},
		&models.CreditChannel{},
	)
	database.Create(&models.CreditChannel{
		Code:         "huabei",
		Name:         "花呗",
		RepaymentDay: 10,
		IsBuiltin:    true,
		Color:        "var(--accent-warm)",
	})
	database.Create(&models.CreditChannel{
		Code:         "baitiao",
		Name:         "京东白条",
		RepaymentDay: 20,
		IsBuiltin:    true,
		Color:        "var(--accent-vermilion)",
	})
	db.DB = database
}

func TestProfileAndConfig(t *testing.T) {
	gin.SetMode(gin.TestMode)
	setupTestFullDB(t)

	r := gin.New()
	r.GET("/api/v1/config", GetConfigV1)
	r.PUT("/api/v1/config", UpdateConfig)
	r.GET("/api/v1/profile", GetCuratorProfile)
	r.PUT("/api/v1/profile", UpdateCuratorProfile)

	// 1. 初始化并获取 Config
	t.Run("Get and update config", func(t *testing.T) {
		req := httptest.NewRequest(http.MethodGet, "/api/v1/config", nil)
		w := httptest.NewRecorder()
		r.ServeHTTP(w, req)
		if w.Code != http.StatusOK {
			t.Fatalf("expected 200, got %d", w.Code)
		}

		// 更新 Config
		updatePayload := models.SiteConfig{
			SiteName:    "新散页 · Living Archive",
			CuratorName: "测试主理人",
		}
		body, _ := json.Marshal(updatePayload)
		putReq := httptest.NewRequest(http.MethodPut, "/api/v1/config", bytes.NewReader(body))
		putReq.Header.Set("Content-Type", "application/json")
		putW := httptest.NewRecorder()
		r.ServeHTTP(putW, putReq)

		if putW.Code != http.StatusOK {
			t.Fatalf("expected 200 for update config, got %d", putW.Code)
		}

		var res struct {
			Data models.SiteConfig `json:"data"`
		}
		_ = json.Unmarshal(putW.Body.Bytes(), &res)
		if res.Data.SiteName != "新散页 · Living Archive" || res.Data.CuratorName != "测试主理人" {
			t.Fatalf("unexpected updated config: %+v", res.Data)
		}
	})

	// 2. 初始化并获取 Profile (含 RG-09 email / github 保存与平铺校验)
	t.Run("Get and update curator profile", func(t *testing.T) {
		// 先初始化一个基础 profile
		initialProfile := models.CuratorProfile{
			Name:        "测试主理人",
			Title:       "数字手艺人",
			Location:    "杭州",
			SocialLinks: `{"github":"https://github.com/example"}`,
			EDCGear:     `[{"name":"机械相机","category":"影像器材"}]`,
		}
		db.DB.Create(&initialProfile)

		req := httptest.NewRequest(http.MethodGet, "/api/v1/profile", nil)
		w := httptest.NewRecorder()
		r.ServeHTTP(w, req)
		if w.Code != http.StatusOK {
			t.Fatalf("expected 200, got %d", w.Code)
		}

		// 更新 profile (传入顶层 email 与 github 验证 RG-09)
		updatePayload := UpdateCuratorProfileRequest{
			Motto:  "真实生活大于一切",
			Bio:    "安静写字的数字工坊",
			Email:  "hello@example.com",
			Github: "https://github.com/new-curator",
		}
		body, _ := json.Marshal(updatePayload)
		putReq := httptest.NewRequest(http.MethodPut, "/api/v1/profile", bytes.NewReader(body))
		putReq.Header.Set("Content-Type", "application/json")
		putW := httptest.NewRecorder()
		r.ServeHTTP(putW, putReq)

		if putW.Code != http.StatusOK {
			t.Fatalf("expected 200 for update profile, got %d", putW.Code)
		}

		var res struct {
			Data models.CuratorProfile `json:"data"`
		}
		_ = json.Unmarshal(putW.Body.Bytes(), &res)
		if res.Data.Motto != "真实生活大于一切" || res.Data.Bio != "安静写字的数字工坊" {
			t.Fatalf("unexpected updated profile: %+v", res.Data)
		}
		if res.Data.Email != "hello@example.com" || res.Data.Github != "https://github.com/new-curator" {
			t.Fatalf("expected flat email and github in response (RG-09), got email=%q github=%q", res.Data.Email, res.Data.Github)
		}
	})
}

func TestSystemLifecycleAndQRAuth(t *testing.T) {
	gin.SetMode(gin.TestMode)
	setupTestFullDB(t)

	r := gin.New()
	r.GET("/api/v1/system/status", GetSystemStatus)
	r.POST("/api/v1/system/bootstrap", BootstrapSystem)
	r.POST("/api/v1/auth/qr/create", CreateQRSession)
	r.POST("/api/v1/auth/qr/authorize", AuthorizeQRSession)
	r.GET("/api/v1/auth/qr/poll/:session_id", PollQRSession)
	r.POST("/api/v1/auth/verify", VerifyPasscode)

	t.Run("1. Initial system status probe", func(t *testing.T) {
		req := httptest.NewRequest(http.MethodGet, "/api/v1/system/status", nil)
		w := httptest.NewRecorder()
		r.ServeHTTP(w, req)

		if w.Code != http.StatusOK {
			t.Fatalf("expected 200, got %d", w.Code)
		}
	})

	t.Run("2. Bootstrap system successfully", func(t *testing.T) {
		config.AppConfig.AdminAPIKey = ""
		reqBody := BootstrapRequest{
			CuratorName:    "测试主理人",
			Location:       "中国 · 杭州",
			MasterPasscode: "my-secret-passcode",
		}
		body, _ := json.Marshal(reqBody)
		req := httptest.NewRequest(http.MethodPost, "/api/v1/system/bootstrap", bytes.NewReader(body))
		req.Header.Set("Content-Type", "application/json")
		w := httptest.NewRecorder()
		r.ServeHTTP(w, req)

		if w.Code != http.StatusOK {
			t.Fatalf("expected 200 for bootstrap, got %d, body: %s", w.Code, w.Body.String())
		}

		var res struct {
			Data struct {
				Token       string `json:"token"`
				CuratorName string `json:"curator_name"`
			} `json:"data"`
		}
		_ = json.Unmarshal(w.Body.Bytes(), &res)
		if res.Data.Token == "" || res.Data.CuratorName != "测试主理人" {
			t.Fatalf("unexpected bootstrap response: %+v", res.Data)
		}
	})

	t.Run("3. Second bootstrap is forbidden (sealed)", func(t *testing.T) {
		reqBody := BootstrapRequest{
			CuratorName: "Attacker",
		}
		body, _ := json.Marshal(reqBody)
		req := httptest.NewRequest(http.MethodPost, "/api/v1/system/bootstrap", bytes.NewReader(body))
		req.Header.Set("Content-Type", "application/json")
		w := httptest.NewRecorder()
		r.ServeHTTP(w, req)

		if w.Code != http.StatusForbidden {
			t.Fatalf("expected 403 Forbidden for second bootstrap, got %d", w.Code)
		}
	})

	var sessID string

	t.Run("4. Create and poll QR session", func(t *testing.T) {
		req := httptest.NewRequest(http.MethodPost, "/api/v1/auth/qr/create", nil)
		w := httptest.NewRecorder()
		r.ServeHTTP(w, req)

		if w.Code != http.StatusOK {
			t.Fatalf("expected 200, got %d", w.Code)
		}

		var res struct {
			Data struct {
				SessionID string `json:"session_id"`
				QRPayload string `json:"qr_payload"`
			} `json:"data"`
		}
		_ = json.Unmarshal(w.Body.Bytes(), &res)
		if res.Data.SessionID == "" {
			t.Fatalf("expected session_id, got empty")
		}
		sessID = res.Data.SessionID

		// Poll before authorization -> pending
		pollReq := httptest.NewRequest(http.MethodGet, "/api/v1/auth/qr/poll/"+sessID, nil)
		pollW := httptest.NewRecorder()
		r.ServeHTTP(pollW, pollReq)

		var pollRes struct {
			Data struct {
				Status string `json:"status"`
			} `json:"data"`
		}
		_ = json.Unmarshal(pollW.Body.Bytes(), &pollRes)
		if pollRes.Data.Status != "pending" {
			t.Fatalf("expected pending status, got %s", pollRes.Data.Status)
		}
	})

	t.Run("5. Authorize QR session and poll token", func(t *testing.T) {
		authBody := AuthorizeQRRequest{
			SessionID: sessID,
		}
		b, _ := json.Marshal(authBody)
		authReq := httptest.NewRequest(http.MethodPost, "/api/v1/auth/qr/authorize", bytes.NewReader(b))
		authReq.Header.Set("Content-Type", "application/json")
		authW := httptest.NewRecorder()
		r.ServeHTTP(authW, authReq)

		if authW.Code != http.StatusOK {
			t.Fatalf("expected 200 for qr authorize, got %d", authW.Code)
		}

		// Poll after authorization -> authorized with token
		pollReq := httptest.NewRequest(http.MethodGet, "/api/v1/auth/qr/poll/"+sessID, nil)
		pollW := httptest.NewRecorder()
		r.ServeHTTP(pollW, pollReq)

		var pollRes struct {
			Data struct {
				Status string `json:"status"`
				Token  string `json:"token"`
			} `json:"data"`
		}
		_ = json.Unmarshal(pollW.Body.Bytes(), &pollRes)
		if pollRes.Data.Status != "authorized" || pollRes.Data.Token == "" {
			t.Fatalf("expected authorized status with token, got %+v", pollRes.Data)
		}
	})

	t.Run("6. Verify passcode login", func(t *testing.T) {
		passReq := VerifyPasscodeRequest{
			Passcode: "my-secret-passcode",
		}
		b, _ := json.Marshal(passReq)
		req := httptest.NewRequest(http.MethodPost, "/api/v1/auth/verify", bytes.NewReader(b))
		req.Header.Set("Content-Type", "application/json")
		w := httptest.NewRecorder()
		r.ServeHTTP(w, req)

		if w.Code != http.StatusOK {
			t.Fatalf("expected 200 for verify passcode, got %d, body: %s", w.Code, w.Body.String())
		}

		var res struct {
			Data struct {
				Token string `json:"token"`
			} `json:"data"`
		}
		_ = json.Unmarshal(w.Body.Bytes(), &res)
		if res.Data.Token == "" {
			t.Fatalf("expected login token, got empty")
		}
	})
}
