package handlers

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"loose-leaf/internal/config"
	"loose-leaf/internal/db"
	"loose-leaf/internal/models"

	"github.com/gin-gonic/gin"
	"github.com/glebarez/sqlite"
	"gorm.io/gorm"
)

func setupTestDB(t *testing.T) {
	dbPath := filepath.Join(t.TempDir(), "test_auth.db")
	database, err := gorm.Open(sqlite.Open(dbPath), &gorm.Config{})
	if err != nil {
		t.Fatalf("failed to open test in-memory db: %v", err)
	}
	_ = database.AutoMigrate(
		&models.SiteConfig{},
		&models.CuratorProfile{},
		&models.Device{},
		&models.PairTicket{},
		&models.Attachment{},
		&models.LifeEntry{},
		&models.Transaction{},
		&models.Wish{},
		&models.CreditChannel{},
		&models.CategoryOption{},
	)
	database.Create(&models.SiteConfig{Initialized: true})
	db.DB = database
}

func TestOAuthSessionLifecycle(t *testing.T) {
	gin.SetMode(gin.TestMode)
	setupTestDB(t)

	prevOIDC := config.AppConfig.OIDC
	config.AppConfig.OIDC.Enabled = true
	t.Cleanup(func() {
		config.AppConfig.OIDC = prevOIDC
	})

	r := gin.New()
	r.POST("/api/v1/auth/oidc/exchange", ExchangeOIDCCode)
	r.POST("/api/v1/auth/pair/confirm", ConfirmPairing)
	r.GET("/api/v1/auth/session", GetSessionStatus)
	r.POST("/api/v1/auth/logout", LogoutSession)
	r.GET("/api/v1/auth/devices", GetTrustedDevices)
	r.PUT("/api/v1/auth/devices/:id", UpdateDeviceName)
	r.DELETE("/api/v1/auth/devices/:id", RevokeDevice)

	var codeStr string

	t.Run("1. Issue one-time OAuth2 exchange code with account identity", func(t *testing.T) {
		rec, err := issueAuthExchangeCode(db.DB, "https://leaf.starryheavens.uk", 90*time.Second, oidcAccountInfo{
			Sub:   "usr_test_001",
			Name:  "Starry",
			Email: "curator@starryheavens.uk",
		})
		if err != nil {
			t.Fatalf("failed to issue auth exchange code: %v", err)
		}
		if !strings.HasPrefix(rec.Ticket, "auth_code_") {
			t.Fatalf("expected auth_code_ prefix, got %q", rec.Ticket)
		}
		codeStr = rec.Ticket
	})

	var accessToken, deviceID string

	t.Run("2. Exchange OAuth2 code for session token & account snapshot", func(t *testing.T) {
		payload := ExchangeOIDCCodeRequest{
			Code:        codeStr,
			DeviceName:  "Test MacBook Pro",
			Platform:    "macos",
			DeviceModel: "Mac15,3",
		}
		body, _ := json.Marshal(payload)
		req := httptest.NewRequest(http.MethodPost, "/api/v1/auth/oidc/exchange", bytes.NewReader(body))
		req.Header.Set("Content-Type", "application/json")
		w := httptest.NewRecorder()
		r.ServeHTTP(w, req)

		if w.Code != http.StatusOK {
			t.Fatalf("expected 200, got %d, body: %s", w.Code, w.Body.String())
		}

		var res struct {
			Code int `json:"code"`
			Data struct {
				SessionID   string `json:"session_id"`
				DeviceID    string `json:"device_id"`
				DeviceName  string `json:"device_name"`
				AccessToken string `json:"access_token"`
				DeviceToken string `json:"device_token"`
				Account     struct {
					Sub   string `json:"sub"`
					Name  string `json:"name"`
					Email string `json:"email"`
				} `json:"account"`
			} `json:"data"`
		}
		_ = json.Unmarshal(w.Body.Bytes(), &res)

		if res.Data.DeviceID == "" || res.Data.AccessToken == "" || res.Data.AccessToken != res.Data.DeviceToken {
			t.Fatalf("expected session/token fields, got: %+v", res.Data)
		}
		if res.Data.Account.Sub != "usr_test_001" || res.Data.Account.Name != "Starry" || res.Data.Account.Email != "curator@starryheavens.uk" {
			t.Fatalf("expected account identity in exchange response, got: %+v", res.Data.Account)
		}

		deviceID = res.Data.DeviceID
		accessToken = res.Data.AccessToken
	})

	t.Run("3. Reusing exchange code is rejected", func(t *testing.T) {
		payload := ExchangeOIDCCodeRequest{
			Code: codeStr,
		}
		body, _ := json.Marshal(payload)
		req := httptest.NewRequest(http.MethodPost, "/api/v1/auth/oidc/exchange", bytes.NewReader(body))
		req.Header.Set("Content-Type", "application/json")
		w := httptest.NewRecorder()
		r.ServeHTTP(w, req)

		if w.Code != http.StatusBadRequest {
			t.Fatalf("expected 400 for already used code, got %d", w.Code)
		}
	})

	t.Run("4. Legacy /auth/pair/confirm alias still exchanges ticket cleanly", func(t *testing.T) {
		ticket2 := models.PairTicket{
			Ticket:    "pair_tk_test_second_ticket",
			ExpiresAt: time.Now().Add(5 * time.Minute),
			Used:      false,
		}
		db.DB.Create(&ticket2)

		payload := ConfirmPairingRequest{
			Ticket:     "pair_tk_test_second_ticket",
			DeviceName: "Test Android",
			Platform:   "android",
		}
		body, _ := json.Marshal(payload)
		req := httptest.NewRequest(http.MethodPost, "/api/v1/auth/pair/confirm", bytes.NewReader(body))
		req.Header.Set("Content-Type", "application/json")
		w := httptest.NewRecorder()
		r.ServeHTTP(w, req)

		if w.Code != http.StatusOK {
			t.Fatalf("expected 200 for legacy confirm alias, got %d, body: %s", w.Code, w.Body.String())
		}
	})

	t.Run("4b. Empty or invalid code is rejected without affecting valid codes", func(t *testing.T) {
		ticket3 := models.PairTicket{
			Ticket:    "auth_code_valid_ticket",
			ExpiresAt: time.Now().Add(5 * time.Minute),
			Used:      false,
		}
		db.DB.Create(&ticket3)

		badPayload := ExchangeOIDCCodeRequest{Code: ""}
		body, _ := json.Marshal(badPayload)
		req := httptest.NewRequest(http.MethodPost, "/api/v1/auth/oidc/exchange", bytes.NewReader(body))
		req.Header.Set("Content-Type", "application/json")
		w := httptest.NewRecorder()
		r.ServeHTTP(w, req)
		if w.Code != http.StatusBadRequest {
			t.Fatalf("expected 400 for empty code, got %d", w.Code)
		}

		var checkTk models.PairTicket
		db.DB.Where("ticket = ?", ticket3.Ticket).First(&checkTk)
		if checkTk.Used {
			t.Fatalf("valid code must remain unused, got used=%v", checkTk.Used)
		}
	})

	t.Run("5. GET /auth/session returns account identity for active Bearer token", func(t *testing.T) {
		req := httptest.NewRequest(http.MethodGet, "/api/v1/auth/session", nil)
		req.Header.Set("Authorization", "Bearer "+accessToken)
		w := httptest.NewRecorder()
		r.ServeHTTP(w, req)

		if w.Code != http.StatusOK {
			t.Fatalf("expected 200, got %d: %s", w.Code, w.Body.String())
		}
		var res struct {
			Data struct {
				IsAdmin bool   `json:"is_admin"`
				Role    string `json:"role"`
				Account struct {
					Sub   string `json:"sub"`
					Name  string `json:"name"`
					Email string `json:"email"`
				} `json:"account"`
			} `json:"data"`
		}
		_ = json.Unmarshal(w.Body.Bytes(), &res)
		if !res.Data.IsAdmin || res.Data.Role != "admin" || res.Data.Account.Name != "Starry" || res.Data.Account.Email != "curator@starryheavens.uk" {
			t.Fatalf("unexpected session payload: %+v", res.Data)
		}
	})

	t.Run("6. List active sessions & rename", func(t *testing.T) {
		req := httptest.NewRequest(http.MethodGet, "/api/v1/auth/devices", nil)
		w := httptest.NewRecorder()
		r.ServeHTTP(w, req)

		if w.Code != http.StatusOK {
			t.Fatalf("expected 200, got %d", w.Code)
		}

		var res struct {
			Code int             `json:"code"`
			Data []models.Device `json:"data"`
		}
		_ = json.Unmarshal(w.Body.Bytes(), &res)
		if len(res.Data) != 2 {
			t.Fatalf("expected 2 active sessions, got %d", len(res.Data))
		}

		renameBody, _ := json.Marshal(map[string]string{"name": "  Studio MacBook Pro M3  "})
		renReq := httptest.NewRequest(http.MethodPut, "/api/v1/auth/devices/"+deviceID, bytes.NewReader(renameBody))
		renReq.Header.Set("Content-Type", "application/json")
		renW := httptest.NewRecorder()
		r.ServeHTTP(renW, renReq)
		if renW.Code != http.StatusOK {
			t.Fatalf("expected 200 on rename, got %d: %s", renW.Code, renW.Body.String())
		}
	})

	t.Run("7. POST /auth/logout revokes Bearer session even if it is the last active session under OIDC", func(t *testing.T) {
		// 先吊销另一台会话，使 accessToken 成为唯一活跃会话
		var all []models.Device
		db.DB.Where("status = ?", "active").Find(&all)
		for _, d := range all {
			if d.DeviceID != deviceID {
				revReq := httptest.NewRequest(http.MethodDelete, "/api/v1/auth/devices/"+d.DeviceID, nil)
				revW := httptest.NewRecorder()
				r.ServeHTTP(revW, revReq)
				if revW.Code != http.StatusOK {
					t.Fatalf("expected 200 revoking secondary session, got %d", revW.Code)
				}
			}
		}

		// 现在登出最后一台活跃会话：OIDC 模式下必须成功并立即使原 Bearer Token 失效
		logoutReq := httptest.NewRequest(http.MethodPost, "/api/v1/auth/logout", nil)
		logoutReq.Header.Set("Authorization", "Bearer "+accessToken)
		logoutW := httptest.NewRecorder()
		r.ServeHTTP(logoutW, logoutReq)
		if logoutW.Code != http.StatusOK {
			t.Fatalf("expected 200 on logout, got %d: %s", logoutW.Code, logoutW.Body.String())
		}

		checkReq := httptest.NewRequest(http.MethodGet, "/api/v1/auth/session", nil)
		checkReq.Header.Set("Authorization", "Bearer "+accessToken)
		checkW := httptest.NewRecorder()
		r.ServeHTTP(checkW, checkReq)
		var checkRes struct {
			Data struct {
				IsAdmin bool `json:"is_admin"`
			} `json:"data"`
		}
		_ = json.Unmarshal(checkW.Body.Bytes(), &checkRes)
		if checkRes.Data.IsAdmin {
			t.Fatalf("expected Bearer token to be invalidated immediately after logout")
		}
	})

	t.Run("8. DetectNetworkCandidates filters out loopback and virtual point-to-point tunnels", func(t *testing.T) {
		candidates := DetectNetworkCandidates("8088")
		for _, c := range candidates {
			if strings.Contains(c.IP, "127.0.0.1") || strings.Contains(c.IP, "172.18.0.1") {
				t.Fatalf("unexpected loopback or virtual tunnel IP in candidates: %+v", c)
			}
		}
	})
}
