package handlers

import (
	"bytes"
	"encoding/base64"
	"encoding/json"
	"image/png"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"loose-leaf/internal/db"
	"loose-leaf/internal/models"

	"github.com/glebarez/sqlite"
	"github.com/gin-gonic/gin"
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

// seedTestAdminDevice 在测试数据库中注册一枚处于 active 状态的受信任设备子钥匙 (BE-18)
func seedTestAdminDevice(rawToken string) {
	now := time.Now()
	db.DB.Create(&models.Device{
		DeviceID:     "dev_web_test_" + generateRandomHex(2),
		Name:         "测试管理员设备",
		Platform:     "web",
		TokenHash:    HashToken(rawToken),
		Status:       "active",
		PairedAt:     now,
		LastActiveAt: now,
	})
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

// TestSystemBootstrapPairingAndVerifyLifecycle 测试两步装帧、配对激活闭环、散页纸墨风二维码与密钥登录 (BE-18, BE-19, BE-20)
func TestSystemBootstrapPairingAndVerifyLifecycle(t *testing.T) {
	gin.SetMode(gin.TestMode)
	setupTestFullDB(t)

	r := gin.New()
	r.GET("/api/v1/system/status", GetSystemStatus)
	r.POST("/api/v1/system/bootstrap", BootstrapSystem)
	r.POST("/api/v1/auth/pair/confirm", ConfirmPairing)
	r.GET("/api/v1/auth/pair/status", PollPairingStatus)
	r.POST("/api/v1/auth/verify", VerifyPasscode)

	var bootstrapTicket string

	t.Run("1. Initial system status probe is uninitialized", func(t *testing.T) {
		req := httptest.NewRequest(http.MethodGet, "/api/v1/system/status", nil)
		w := httptest.NewRecorder()
		r.ServeHTTP(w, req)

		if w.Code != http.StatusOK {
			t.Fatalf("expected 200, got %d", w.Code)
		}
		var res struct {
			Data struct {
				Initialized    bool `json:"initialized"`
				HasPasscodeSet bool `json:"has_passcode_set"`
			} `json:"data"`
		}
		_ = json.Unmarshal(w.Body.Bytes(), &res)
		if res.Data.Initialized || res.Data.HasPasscodeSet {
			t.Fatalf("expected uninitialized and no passcode set, got %+v", res.Data)
		}
	})

	t.Run("2. Bootstrap rejects empty or mismatched confirm_passcode (BE-19)", func(t *testing.T) {
		badReq := BootstrapRequest{
			CuratorName:     "测试主理人",
			Location:        "中国 · 杭州",
			MasterPasscode:  "my-secret-passcode",
			ConfirmPasscode: "different-passcode",
		}
		body, _ := json.Marshal(badReq)
		req := httptest.NewRequest(http.MethodPost, "/api/v1/system/bootstrap", bytes.NewReader(body))
		req.Header.Set("Content-Type", "application/json")
		w := httptest.NewRecorder()
		r.ServeHTTP(w, req)

		if w.Code != http.StatusBadRequest {
			t.Fatalf("expected 400 for mismatched confirm_passcode, got %d", w.Code)
		}
	})

	t.Run("3. Bootstrap Step 1 saves passcode hash, keeps initialized=false, and returns paper-ink QR ticket (BE-19, BE-20)", func(t *testing.T) {
		reqBody := BootstrapRequest{
			CuratorName:     "测试主理人",
			Location:        "中国 · 杭州",
			MasterPasscode:  "my-secret-passcode",
			ConfirmPasscode: "my-secret-passcode",
			HubURL:          "http://192.168.1.10:5174",
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
				CuratorName   string `json:"curator_name"`
				Location      string `json:"location"`
				Ticket        string `json:"ticket"`
				PinCode       string `json:"pin_code"`
				HubURL        string `json:"hub_url"`
				QRPayload     string `json:"qr_payload"`
				QRCodeDataURL string `json:"qr_code_data_url"`
			} `json:"data"`
		}
		_ = json.Unmarshal(w.Body.Bytes(), &res)
		if res.Data.Ticket == "" || res.Data.PinCode == "" || res.Data.CuratorName != "测试主理人" {
			t.Fatalf("unexpected bootstrap response: %+v", res.Data)
		}
		bootstrapTicket = res.Data.Ticket

		// 验证此时 SiteConfig.Initialized 仍为 false (等待首台设备握手后才激活 BE-19)
		var cfg models.SiteConfig
		db.DB.First(&cfg)
		if cfg.Initialized {
			t.Fatalf("BE-19 violation: SiteConfig.Initialized must remain false before device pairing confirmation")
		}
		if cfg.MasterPasscode != HashToken("my-secret-passcode") {
			t.Fatalf("expected SHA-256 hashed master_passcode in DB")
		}

		// 验证 BE-20 散页纸墨风二维码配色 (#181716 墨色与 #F9F8F5 暖纸背景)
		b64 := strings.TrimPrefix(res.Data.QRCodeDataURL, "data:image/png;base64,")
		pngBytes, err := base64.StdEncoding.DecodeString(b64)
		if err != nil {
			t.Fatalf("failed to decode QR base64: %v", err)
		}
		img, err := png.Decode(bytes.NewReader(pngBytes))
		if err != nil {
			t.Fatalf("failed to decode QR PNG: %v", err)
		}
		// 左上角 (0, 0) 为二维码静区背景色，应为 #F9F8F5 (249, 248, 245)
		r0, g0, b0, _ := img.At(0, 0).RGBA()
		if uint8(r0>>8) != 249 || uint8(g0>>8) != 248 || uint8(b0>>8) != 245 {
			t.Fatalf("BE-20 QR background color mismatch: got RGB(%d,%d,%d), expected RGB(249,248,245)", r0>>8, g0>>8, b0>>8)
		}
	})

	t.Run("4. Poll pairing status before confirmation returns pending and initialized=false (BE-19)", func(t *testing.T) {
		req := httptest.NewRequest(http.MethodGet, "/api/v1/auth/pair/status?ticket="+bootstrapTicket, nil)
		w := httptest.NewRecorder()
		r.ServeHTTP(w, req)

		if w.Code != http.StatusOK {
			t.Fatalf("expected 200, got %d", w.Code)
		}
		var res struct {
			Data struct {
				Status      string `json:"status"`
				Initialized bool   `json:"initialized"`
			} `json:"data"`
		}
		_ = json.Unmarshal(w.Body.Bytes(), &res)
		if res.Data.Status != "pending" || res.Data.Initialized {
			t.Fatalf("expected pending and initialized=false, got %+v", res.Data)
		}
	})

	t.Run("5. Confirm pairing activates initialized=true and PollPairingStatus returns companion web_token once (BE-19)", func(t *testing.T) {
		confirmPayload := ConfirmPairingRequest{
			Ticket:     bootstrapTicket,
			DeviceName: "iPhone 16 Pro",
			Platform:   "ios",
		}
		body, _ := json.Marshal(confirmPayload)
		req := httptest.NewRequest(http.MethodPost, "/api/v1/auth/pair/confirm", bytes.NewReader(body))
		req.Header.Set("Content-Type", "application/json")
		w := httptest.NewRecorder()
		r.ServeHTTP(w, req)

		if w.Code != http.StatusOK {
			t.Fatalf("expected 200 on pair confirm, got %d: %s", w.Code, w.Body.String())
		}

		var confirmRes struct {
			Data struct {
				DeviceID    string `json:"device_id"`
				DeviceToken string `json:"device_token"`
				Initialized bool   `json:"initialized"`
			} `json:"data"`
		}
		_ = json.Unmarshal(w.Body.Bytes(), &confirmRes)
		if !confirmRes.Data.Initialized || !strings.HasPrefix(confirmRes.Data.DeviceID, "dev_ios_") || !strings.HasPrefix(confirmRes.Data.DeviceToken, "lldt_") {
			t.Fatalf("unexpected confirm pairing response: %+v", confirmRes.Data)
		}

		// 轮询状态：应返回 status="paired", initialized=true 以及伴生 web_token
		pollReq := httptest.NewRequest(http.MethodGet, "/api/v1/auth/pair/status?ticket="+bootstrapTicket, nil)
		pollW := httptest.NewRecorder()
		r.ServeHTTP(pollW, pollReq)

		var pollRes struct {
			Data struct {
				Status      string `json:"status"`
				Initialized bool   `json:"initialized"`
				WebToken    string `json:"web_token"`
			} `json:"data"`
		}
		_ = json.Unmarshal(pollW.Body.Bytes(), &pollRes)
		if pollRes.Data.Status != "paired" || !pollRes.Data.Initialized || !strings.HasPrefix(pollRes.Data.WebToken, "lldt_") {
			t.Fatalf("expected paired status with companion web_token, got %+v", pollRes.Data)
		}

		// 二次轮询：web_token 已单次销毁，不再返回明文
		pollReq2 := httptest.NewRequest(http.MethodGet, "/api/v1/auth/pair/status?ticket="+bootstrapTicket, nil)
		pollW2 := httptest.NewRecorder()
		r.ServeHTTP(pollW2, pollReq2)
		var pollRes2 struct {
			Data struct {
				WebToken string `json:"web_token"`
			} `json:"data"`
		}
		_ = json.Unmarshal(pollW2.Body.Bytes(), &pollRes2)
		if pollRes2.Data.WebToken != "" {
			t.Fatalf("expected web_token to be cleared after first read, got %q", pollRes2.Data.WebToken)
		}
	})

	t.Run("6. Second bootstrap after activation is forbidden (403 sealed)", func(t *testing.T) {
		reqBody := BootstrapRequest{
			CuratorName:     "Attacker",
			MasterPasscode:  "hack",
			ConfirmPasscode: "hack",
		}
		body, _ := json.Marshal(reqBody)
		req := httptest.NewRequest(http.MethodPost, "/api/v1/system/bootstrap", bytes.NewReader(body))
		req.Header.Set("Content-Type", "application/json")
		w := httptest.NewRecorder()
		r.ServeHTTP(w, req)

		if w.Code != http.StatusForbidden {
			t.Fatalf("expected 403 Forbidden for second bootstrap after activation, got %d", w.Code)
		}
	})

	t.Run("7. Verify passcode login issues structured device_id and lldt_ token (BE-18)", func(t *testing.T) {
		// 错误密钥返回 401
		wrongBody, _ := json.Marshal(VerifyPasscodeRequest{Passcode: "wrong-pass"})
		wrongReq := httptest.NewRequest(http.MethodPost, "/api/v1/auth/verify", bytes.NewReader(wrongBody))
		wrongReq.Header.Set("Content-Type", "application/json")
		wrongW := httptest.NewRecorder()
		r.ServeHTTP(wrongW, wrongReq)
		if wrongW.Code != http.StatusUnauthorized {
			t.Fatalf("expected 401 for wrong passcode, got %d", wrongW.Code)
		}

		// 正确密钥返回 200 及 token, device_id, device_name
		passReq := VerifyPasscodeRequest{
			Passcode:   "my-secret-passcode",
			DeviceName: "Safari · macOS",
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
				Token      string `json:"token"`
				DeviceID   string `json:"device_id"`
				DeviceName string `json:"device_name"`
			} `json:"data"`
		}
		_ = json.Unmarshal(w.Body.Bytes(), &res)
		if !strings.HasPrefix(res.Data.Token, "lldt_") || !strings.HasPrefix(res.Data.DeviceID, "dev_web_") || res.Data.DeviceName != "Safari · macOS" {
			t.Fatalf("unexpected verify response: %+v", res.Data)
		}
		initialDeviceID := res.Data.DeviceID

		// 8. 相同浏览器重复输入密钥登录，应复用既有设备记录，杜绝无界膨胀 (RC-82)
		wSecond := httptest.NewRecorder()
		reqSecond := httptest.NewRequest(http.MethodPost, "/api/v1/auth/verify", bytes.NewReader(b))
		reqSecond.Header.Set("Content-Type", "application/json")
		r.ServeHTTP(wSecond, reqSecond)
		if wSecond.Code != http.StatusOK {
			t.Fatalf("expected 200 on repeated passcode verify, got %d", wSecond.Code)
		}
		var resSecond struct {
			Data struct {
				Token      string `json:"token"`
				DeviceID   string `json:"device_id"`
				DeviceName string `json:"device_name"`
			} `json:"data"`
		}
		_ = json.Unmarshal(wSecond.Body.Bytes(), &resSecond)
		if resSecond.Data.DeviceID != initialDeviceID {
			t.Fatalf("expected reused device_id %s, got new device_id %s (RC-82)", initialDeviceID, resSecond.Data.DeviceID)
		}

		var devCount int64
		db.DB.Model(&models.Device{}).Where("platform = ? AND name = ?", "web", "Safari · macOS").Count(&devCount)
		if devCount != 1 {
			t.Fatalf("expected exactly 1 device record for 'Safari · macOS', got %d (RC-82)", devCount)
		}
	})
}
