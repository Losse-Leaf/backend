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
	"github.com/glebarez/sqlite"
	"gorm.io/gorm"
)

func setupTestDB(t *testing.T) {
	database, err := gorm.Open(sqlite.Open(":memory:"), &gorm.Config{})
	if err != nil {
		t.Fatalf("failed to open test in-memory db: %v", err)
	}
	_ = database.AutoMigrate(&models.Device{}, &models.PairTicket{}, &models.SiteConfig{})
	database.Create(&models.SiteConfig{Initialized: true})
	db.DB = database
}

func TestDevicePairingLifecycle(t *testing.T) {
	gin.SetMode(gin.TestMode)
	setupTestDB(t)

	r := gin.New()
	r.POST("/api/v1/auth/pair/init", InitPairing)
	r.POST("/api/v1/auth/pair/confirm", ConfirmPairing)
	r.GET("/api/v1/auth/devices", GetTrustedDevices)
	r.DELETE("/api/v1/auth/devices/:id", RevokeDevice)

	var ticketStr, pinCode string

	t.Run("1. Init pairing ticket", func(t *testing.T) {
		req := httptest.NewRequest(http.MethodPost, "/api/v1/auth/pair/init", nil)
		w := httptest.NewRecorder()
		r.ServeHTTP(w, req)

		if w.Code != http.StatusOK {
			t.Fatalf("expected 200, got %d", w.Code)
		}

		var res struct {
			Code int `json:"code"`
			Data struct {
				Ticket           string `json:"ticket"`
				PinCode          string `json:"pin_code"`
				ExpiresInSeconds int    `json:"expires_in_seconds"`
				QRPayload        string `json:"qr_payload"`
			} `json:"data"`
		}
		if err := json.Unmarshal(w.Body.Bytes(), &res); err != nil {
			t.Fatalf("failed to decode response: %v", err)
		}

		if res.Data.Ticket == "" || len(res.Data.PinCode) != 6 {
			t.Fatalf("invalid ticket or pin code: %+v", res.Data)
		}

		ticketStr = res.Data.Ticket
		pinCode = res.Data.PinCode
	})

	var deviceToken, deviceID string

	t.Run("2. Confirm pairing with ticket", func(t *testing.T) {
		payload := ConfirmPairingRequest{
			Ticket:      ticketStr,
			DeviceName:  "Test MacBook Pro",
			Platform:    "macos",
			DeviceModel: "Mac15,3",
		}
		body, _ := json.Marshal(payload)
		req := httptest.NewRequest(http.MethodPost, "/api/v1/auth/pair/confirm", bytes.NewReader(body))
		req.Header.Set("Content-Type", "application/json")
		w := httptest.NewRecorder()
		r.ServeHTTP(w, req)

		if w.Code != http.StatusOK {
			t.Fatalf("expected 200, got %d, body: %s", w.Code, w.Body.String())
		}

		var res struct {
			Code int `json:"code"`
			Data struct {
				DeviceID    string `json:"device_id"`
				DeviceName  string `json:"device_name"`
				DeviceToken string `json:"device_token"`
			} `json:"data"`
		}
		_ = json.Unmarshal(w.Body.Bytes(), &res)

		if res.Data.DeviceID == "" || res.Data.DeviceToken == "" {
			t.Fatalf("expected device_id and device_token, got: %+v", res.Data)
		}

		deviceID = res.Data.DeviceID
		deviceToken = res.Data.DeviceToken
	})

	t.Run("3. Reusing ticket is rejected", func(t *testing.T) {
		payload := ConfirmPairingRequest{
			Ticket: ticketStr,
		}
		body, _ := json.Marshal(payload)
		req := httptest.NewRequest(http.MethodPost, "/api/v1/auth/pair/confirm", bytes.NewReader(body))
		req.Header.Set("Content-Type", "application/json")
		w := httptest.NewRecorder()
		r.ServeHTTP(w, req)

		if w.Code != http.StatusBadRequest {
			t.Fatalf("expected 400 for already used ticket, got %d", w.Code)
		}
	})

	t.Run("4. Confirm pairing with PIN code on second ticket", func(t *testing.T) {
		// 先初始化第二个工单
		ticket2 := models.PairTicket{
			Ticket:    "pair_tk_test_pin_ticket",
			PinCode:   "888666",
			ExpiresAt: time.Now().Add(5 * time.Minute),
			Used:      false,
		}
		db.DB.Create(&ticket2)

		payload := ConfirmPairingRequest{
			PinCode:    "888666",
			DeviceName: "Test iPhone 16",
			Platform:   "ios",
		}
		body, _ := json.Marshal(payload)
		req := httptest.NewRequest(http.MethodPost, "/api/v1/auth/pair/confirm", bytes.NewReader(body))
		req.Header.Set("Content-Type", "application/json")
		w := httptest.NewRecorder()
		r.ServeHTTP(w, req)

		if w.Code != http.StatusOK {
			t.Fatalf("expected 200 for PIN confirm, got %d, body: %s", w.Code, w.Body.String())
		}
	})

	// BE-26：纯 PIN 握手不定位工单，故错误 PIN 不再跨工单累加（否则 5 次错误即可作废全部工单形成 DoS）。
	// 防爆破改由 ConfirmPairing 入口的 IP 级限速（30 次/分钟）兜底；带 ticket 的握手仍按工单计次作废。
	t.Run("4b. Pure-PIN failures do not invalidate other tickets (rate-limit is the brute-force guard)", func(t *testing.T) {
		ticket3 := models.PairTicket{
			Ticket:    "pair_tk_bruteforce_test",
			PinCode:   "999111",
			ExpiresAt: time.Now().Add(5 * time.Minute),
			Used:      false,
		}
		db.DB.Create(&ticket3)

		for i := 0; i < 5; i++ {
			badPayload := ConfirmPairingRequest{PinCode: "000000"}
			body, _ := json.Marshal(badPayload)
			req := httptest.NewRequest(http.MethodPost, "/api/v1/auth/pair/confirm", bytes.NewReader(body))
			req.Header.Set("Content-Type", "application/json")
			w := httptest.NewRecorder()
			r.ServeHTTP(w, req)
			if w.Code != http.StatusBadRequest {
				t.Fatalf("expected 400 for incorrect pin attempt %d, got %d", i+1, w.Code)
			}
		}

		var checkTk models.PairTicket
		db.DB.Where("ticket = ?", ticket3.Ticket).First(&checkTk)
		if checkTk.Used || checkTk.FailedAttempts != 0 {
			t.Fatalf("pure-PIN failures must not void tickets, got used=%v failed_attempts=%d", checkTk.Used, checkTk.FailedAttempts)
		}

		// 正确 PIN 仍可正常配对（工单未被误伤）
		correctPayload := ConfirmPairingRequest{PinCode: "999111"}
		body, _ := json.Marshal(correctPayload)
		req := httptest.NewRequest(http.MethodPost, "/api/v1/auth/pair/confirm", bytes.NewReader(body))
		req.Header.Set("Content-Type", "application/json")
		w := httptest.NewRecorder()
		r.ServeHTTP(w, req)
		if w.Code != http.StatusOK {
			t.Fatalf("expected 200 for correct pin after failed attempts, got %d: %s", w.Code, w.Body.String())
		}

		// 清理本子测试新建的设备，避免影响后续设备计数断言
		db.DB.Where("platform = ?", "device").Delete(&models.Device{})
	})

	t.Run("4c. Ticket-scoped handshake still counts failures and voids ticket after 5 attempts", func(t *testing.T) {
		ticket4 := models.PairTicket{
			Ticket:    "pair_tk_scoped_test",
			PinCode:   "999222",
			ExpiresAt: time.Now().Add(5 * time.Minute),
			Used:      false,
		}
		db.DB.Create(&ticket4)

		for i := 0; i < 5; i++ {
			badPayload := ConfirmPairingRequest{Ticket: ticket4.Ticket, PinCode: "000000"}
			body, _ := json.Marshal(badPayload)
			req := httptest.NewRequest(http.MethodPost, "/api/v1/auth/pair/confirm", bytes.NewReader(body))
			req.Header.Set("Content-Type", "application/json")
			w := httptest.NewRecorder()
			r.ServeHTTP(w, req)
			if w.Code != http.StatusBadRequest {
				t.Fatalf("expected 400 for incorrect pin attempt %d, got %d", i+1, w.Code)
			}
		}

		var checkTk models.PairTicket
		db.DB.Where("ticket = ?", ticket4.Ticket).First(&checkTk)
		if !checkTk.Used || checkTk.FailedAttempts != 5 {
			t.Fatalf("expected scoped ticket used=true failed_attempts=5, got used=%v failed_attempts=%d", checkTk.Used, checkTk.FailedAttempts)
		}
	})

	t.Run("5. List trusted devices", func(t *testing.T) {
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
			t.Fatalf("expected 2 devices, got %d", len(res.Data))
		}
	})

	t.Run("6. Revoke device and verify it is excluded from GetTrustedDevices", func(t *testing.T) {
		req := httptest.NewRequest(http.MethodDelete, "/api/v1/auth/devices/"+deviceID, nil)
		w := httptest.NewRecorder()
		r.ServeHTTP(w, req)

		if w.Code != http.StatusOK {
			t.Fatalf("expected 200, got %d", w.Code)
		}

		var device models.Device
		db.DB.Where("device_id = ?", deviceID).First(&device)
		if device.Status != "revoked" {
			t.Fatalf("expected status 'revoked', got '%s'", device.Status)
		}

		// Verify GetTrustedDevices only returns active devices
		listReq := httptest.NewRequest(http.MethodGet, "/api/v1/auth/devices", nil)
		listW := httptest.NewRecorder()
		r.ServeHTTP(listW, listReq)

		var res struct {
			Code int             `json:"code"`
			Data []models.Device `json:"data"`
		}
		_ = json.Unmarshal(listW.Body.Bytes(), &res)
		if len(res.Data) != 1 {
			t.Fatalf("expected 1 active device after revocation, got %d", len(res.Data))
		}
		if res.Data[0].DeviceID == deviceID {
			t.Fatalf("expected revoked device %s to be excluded from trusted devices list", deviceID)
		}
	})

	_ = deviceToken
	_ = pinCode
}
