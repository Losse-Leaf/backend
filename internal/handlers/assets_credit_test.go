package handlers

import (
	"bytes"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/gin-gonic/gin"
	"loose-leaf/internal/config"
	"loose-leaf/internal/db"
	"loose-leaf/internal/middleware"
	"loose-leaf/internal/models"
)

func TestCalculateElapsedInstallmentMonthsUsesBeijingDate(t *testing.T) {
	purchase := time.Date(2026, 6, 2, 0, 0, 0, 0, config.BeijingLocation)
	// 北京时间 9/25 00:30（UTC 仍是 9/24）：9/25 这一期已经到期，应计 4 期。
	checkTime := time.Date(2026, 9, 25, 0, 30, 0, 0, config.BeijingLocation).UTC()
	paid, remain := CalculateElapsedInstallmentMonths(purchase, 12, 25, checkTime)
	if paid != 4 || remain != 8 {
		t.Fatalf("UTC 服务器应按北京时间切分还款日: paid=%d remain=%d, want 4/8", paid, remain)
	}
}

func TestAssetsCreditAndDesensitization(t *testing.T) {
	gin.SetMode(gin.TestMode)
	setupTestFullDB(t)

	// 注册测试受信任设备凭证 (BE-18)
	testMasterKey := "lldt_test_device_token_123"
	seedTestAdminDevice(testMasterKey)

	r := gin.New()
	v1 := r.Group("/api/v1")
	{
		// 公开路由
		v1.GET("/profile", GetCuratorProfile)
		v1.GET("/wishes", GetWishes)
		v1.POST("/wishes", CreateWish)
		v1.POST("/wishes/:id/resolve", ResolveWish)

		// 管理员鉴权受保护路由组 (RC-53)
		write := v1.Group("")
		write.Use(middleware.AdminAuthRequired())
		{
			write.GET("/transactions", GetTransactions)
			write.GET("/transactions/summary", GetTransactionSummary)
			write.POST("/transactions", CreateTransaction)

			write.GET("/assets/overview", GetAssetsOverview)
			write.POST("/assets/gear", AddGearAsset)
			write.POST("/assets/gear/:id/sell", SellGearAsset)
			write.DELETE("/assets/gear/:id", DeleteGearAsset)

			write.GET("/credit-channels", GetCreditChannels)
			write.POST("/credit-channels", CreateCreditChannel)
			write.PUT("/credit-channels/:id", UpdateCreditChannel)
			write.DELETE("/credit-channels/:id", DeleteCreditChannel)
		}
	}

	// 1. 信贷渠道管理 (Credit Channels CRUD)
	t.Run("Credit Channels CRUD", func(t *testing.T) {
		// 1.1 未授权访问返回 401
		w := httptest.NewRecorder()
		req, _ := http.NewRequest(http.MethodGet, "/api/v1/credit-channels", nil)
		r.ServeHTTP(w, req)
		if w.Code != http.StatusUnauthorized {
			t.Fatalf("expected 401 on unauthorized credit-channels, got %d", w.Code)
		}

		// 1.2 带 Token 查询内置渠道
		w = httptest.NewRecorder()
		req, _ = http.NewRequest(http.MethodGet, "/api/v1/credit-channels", nil)
		req.Header.Set("Authorization", "Bearer "+testMasterKey)
		r.ServeHTTP(w, req)
		if w.Code != http.StatusOK {
			t.Fatalf("expected 200 on authorized credit-channels, got %d", w.Code)
		}
		var listResp map[string]interface{}
		_ = json.Unmarshal(w.Body.Bytes(), &listResp)
		channels := listResp["data"].([]interface{})
		if len(channels) < 2 {
			t.Fatalf("expected at least 2 builtin channels, got %d", len(channels))
		}

		// 1.3 新建自定义渠道 (招行信用卡，15 号还款)
		createPayload := CreditChannelRequest{
			Code:         "cmb_card",
			Name:         "招行信用卡",
			RepaymentDay: 15,
			Color:        "var(--accent-indigo)",
		}
		body, _ := json.Marshal(createPayload)
		w = httptest.NewRecorder()
		req, _ = http.NewRequest(http.MethodPost, "/api/v1/credit-channels", bytes.NewBuffer(body))
		req.Header.Set("Authorization", "Bearer "+testMasterKey)
		req.Header.Set("Content-Type", "application/json")
		r.ServeHTTP(w, req)
		if w.Code != http.StatusOK {
			t.Fatalf("expected 200 on create credit channel, got %d: %s", w.Code, w.Body.String())
		}
		var chResp map[string]interface{}
		_ = json.Unmarshal(w.Body.Bytes(), &chResp)
		createdCh := chResp["data"].(map[string]interface{})
		createdID := uint(createdCh["id"].(float64))

		// 1.4 修改自定义渠道还款日为 18 号
		updatePayload := CreditChannelRequest{
			Name:         "招行经典白",
			RepaymentDay: 18,
		}
		body, _ = json.Marshal(updatePayload)
		w = httptest.NewRecorder()
		req, _ = http.NewRequest(http.MethodPut, fmt.Sprintf("/api/v1/credit-channels/%d", createdID), bytes.NewBuffer(body))
		req.Header.Set("Authorization", "Bearer "+testMasterKey)
		req.Header.Set("Content-Type", "application/json")
		r.ServeHTTP(w, req)
		if w.Code != http.StatusOK {
			t.Fatalf("expected 200 on update credit channel, got %d", w.Code)
		}

		// 1.4b 修改渠道 code 并同步迁移关联账目
		db.DB.Create(&models.Transaction{
			Title:                  "招行买书",
			Amount:                 20000,
			Type:                   "expense",
			IsInstallment:          true,
			InstallmentChannelCode: "cmb_card",
			OccurredAt:             time.Now(),
		})
		updateCodePayload := CreditChannelRequest{
			Code:         "cmb_visa",
			Name:         "招行经典白 VISA",
			RepaymentDay: 18,
		}
		body, _ = json.Marshal(updateCodePayload)
		w = httptest.NewRecorder()
		req, _ = http.NewRequest(http.MethodPut, fmt.Sprintf("/api/v1/credit-channels/%d", createdID), bytes.NewBuffer(body))
		req.Header.Set("Authorization", "Bearer "+testMasterKey)
		req.Header.Set("Content-Type", "application/json")
		r.ServeHTTP(w, req)
		if w.Code != http.StatusOK {
			t.Fatalf("expected 200 updating channel code: %d", w.Code)
		}
		var checkTx models.Transaction
		db.DB.Where("title = ?", "招行买书").First(&checkTx)
		if checkTx.InstallmentChannelCode != "cmb_visa" {
			t.Fatalf("expected transaction channel code to be migrated to cmb_visa, got %s", checkTx.InstallmentChannelCode)
		}

		// 1.5 尝试删除内置渠道（应该被拒绝 400）
		w = httptest.NewRecorder()
		req, _ = http.NewRequest(http.MethodDelete, "/api/v1/credit-channels/1", nil)
		req.Header.Set("Authorization", "Bearer "+testMasterKey)
		r.ServeHTTP(w, req)
		if w.Code != http.StatusBadRequest {
			t.Fatalf("expected 400 when deleting builtin channel, got %d", w.Code)
		}

		// 1.6 删除自定义渠道
		w = httptest.NewRecorder()
		req, _ = http.NewRequest(http.MethodDelete, fmt.Sprintf("/api/v1/credit-channels/%d", createdID), nil)
		req.Header.Set("Authorization", "Bearer "+testMasterKey)
		r.ServeHTTP(w, req)
		if w.Code != http.StatusOK {
			t.Fatalf("expected 200 when deleting custom channel, got %d", w.Code)
		}
	})

	// 2. 访客端绝对隔离与设备脱敏 (Visitor Zero-Knowledge & Desensitization RC-53)
	t.Run("Visitor Desensitization and Auth Isolation", func(t *testing.T) {
		// 初始化主理人并植入带有价格的随身设备
		var profile models.CuratorProfile
		db.DB.FirstOrCreate(&profile)
		initialGears := []models.GearItem{
			{
				ID:             "gear_test_01",
				Name:           "Leica M11",
				Spec:           "银色旁轴机身",
				Category:       "摄影器材",
				Status:         "active",
				PriceCents:     6800000, // 68,000 元
				PurchaseDate:   "2026-05-01",
				SoldPriceCents: 0,
			},
		}
		gearJSON, _ := json.Marshal(initialGears)
		profile.EDCGear = string(gearJSON)
		db.DB.Save(&profile)

		// 2.1 访客 GET /api/v1/profile 应该严格脱敏 price_cents 与 sold_price_cents
		w := httptest.NewRecorder()
		req, _ := http.NewRequest(http.MethodGet, "/api/v1/profile", nil)
		r.ServeHTTP(w, req)
		if w.Code != http.StatusOK {
			t.Fatalf("expected 200 on guest get profile, got %d", w.Code)
		}
		var guestResp map[string]interface{}
		_ = json.Unmarshal(w.Body.Bytes(), &guestResp)
		guestData := guestResp["data"].(map[string]interface{})
		guestGearList := guestData["edc_gear"].([]interface{})
		if len(guestGearList) != 1 {
			t.Fatalf("expected 1 gear, got %d", len(guestGearList))
		}
		firstGear := guestGearList[0].(map[string]interface{})
		if _, exists := firstGear["price_cents"]; exists {
			t.Fatalf("SECURITY VIOLATION: visitor must not see price_cents! got %v", firstGear["price_cents"])
		}
		if _, exists := firstGear["sold_price_cents"]; exists {
			t.Fatalf("SECURITY VIOLATION: visitor must not see sold_price_cents!")
		}

		// 2.2 管理员 GET /api/v1/profile 可以看到完整的 price_cents
		w = httptest.NewRecorder()
		req, _ = http.NewRequest(http.MethodGet, "/api/v1/profile", nil)
		req.Header.Set("Authorization", "Bearer "+testMasterKey)
		r.ServeHTTP(w, req)
		if w.Code != http.StatusOK {
			t.Fatalf("expected 200 on admin get profile, got %d", w.Code)
		}
		var adminResp map[string]interface{}
		_ = json.Unmarshal(w.Body.Bytes(), &adminResp)
		adminData := adminResp["data"].(map[string]interface{})
		adminGearList := adminData["edc_gear"].([]interface{})
		adminFirstGear := adminGearList[0].(map[string]interface{})
		if int64(adminFirstGear["price_cents"].(float64)) != 6800000 {
			t.Fatalf("expected admin to see 6800000 price_cents, got %v", adminFirstGear["price_cents"])
		}

		// 2.3 访客请求 GET /api/v1/transactions 必须返回 401 Unauthorized (RC-53)
		w = httptest.NewRecorder()
		req, _ = http.NewRequest(http.MethodGet, "/api/v1/transactions", nil)
		r.ServeHTTP(w, req)
		if w.Code != http.StatusUnauthorized {
			t.Fatalf("SECURITY VIOLATION: guest must get 401 on transactions, got %d", w.Code)
		}

		// 2.4 访客请求 GET /api/v1/assets/overview 必须返回 401 Unauthorized (RC-53)
		w = httptest.NewRecorder()
		req, _ = http.NewRequest(http.MethodGet, "/api/v1/assets/overview", nil)
		r.ServeHTTP(w, req)
		if w.Code != http.StatusUnauthorized {
			t.Fatalf("SECURITY VIOLATION: guest must get 401 on assets overview, got %d", w.Code)
		}
	})

	// 3. 设备全生命周期双向绑定与资产大盘 (Gear Lifecycle & Assets Overview RC-53)
	t.Run("Gear Lifecycle and Assets Overview", func(t *testing.T) {
		// 3.1 录入新设备并同步生成支出账单 (买入即记账)
		addReq := AddGearAssetRequest{
			Name:              "Mac Studio M2 Max",
			Category:          "数码设备",
			Spec:              "32GB + 1TB",
			PriceCents:        1649900,
			PurchaseDate:      "2026-10-10",
			PaymentMethod:     "支付宝",
			Note:              "桌面主力生产力",
			CreateTransaction: true,
		}
		body, _ := json.Marshal(addReq)
		w := httptest.NewRecorder()
		req, _ := http.NewRequest(http.MethodPost, "/api/v1/assets/gear", bytes.NewBuffer(body))
		req.Header.Set("Authorization", "Bearer "+testMasterKey)
		req.Header.Set("Content-Type", "application/json")
		r.ServeHTTP(w, req)
		if w.Code != http.StatusOK {
			t.Fatalf("expected 200 on add gear asset, got %d", w.Code)
		}
		var addResp map[string]interface{}
		_ = json.Unmarshal(w.Body.Bytes(), &addResp)
		gearData := addResp["data"].(map[string]interface{})
		gearID := gearData["id"].(string)

		// 验证支出账单已自动创建
		var txCount int64
		db.DB.Model(&models.Transaction{}).Where("title = ?", "购置设备: Mac Studio M2 Max").Count(&txCount)
		if txCount != 1 {
			t.Fatalf("expected 1 auto created expense transaction, got %d", txCount)
		}

		// 3.2 资产大盘看板查询
		w = httptest.NewRecorder()
		req, _ = http.NewRequest(http.MethodGet, "/api/v1/assets/overview", nil)
		req.Header.Set("Authorization", "Bearer "+testMasterKey)
		r.ServeHTTP(w, req)
		if w.Code != http.StatusOK {
			t.Fatalf("expected 200 on assets overview, got %d", w.Code)
		}
		var overviewResp map[string]interface{}
		_ = json.Unmarshal(w.Body.Bytes(), &overviewResp)
		overviewData := overviewResp["data"].(map[string]interface{})
		gearSummary := overviewData["gear_summary"].(map[string]interface{})
		if int(gearSummary["active_count"].(float64)) < 1 {
			t.Fatalf("expected active_count >= 1, got %v", gearSummary["active_count"])
		}

		// 3.3 设备出二手转售并自动生成回血流水 (卖出自动生成回血收入账单)
		sellReq := SellGearAssetRequest{
			SoldPriceCents: 1250000, // 12,500 元
			SoldDate:       "2026-10-10",
			Note:           "转售给同城朋友",
		}
		body, _ = json.Marshal(sellReq)
		w = httptest.NewRecorder()
		req, _ = http.NewRequest(http.MethodPost, fmt.Sprintf("/api/v1/assets/gear/%s/sell", gearID), bytes.NewBuffer(body))
		req.Header.Set("Authorization", "Bearer "+testMasterKey)
		req.Header.Set("Content-Type", "application/json")
		r.ServeHTTP(w, req)
		if w.Code != http.StatusOK {
			t.Fatalf("expected 200 on sell gear asset, got %d: %s", w.Code, w.Body.String())
		}

		// 验证自动生成了一条收入流水 (分类: 二手回血)
		var incomeCount int64
		db.DB.Model(&models.Transaction{}).
			Where("type = 'income' AND category = '二手回血' AND amount = 1250000").
			Count(&incomeCount)
		if incomeCount != 1 {
			t.Fatalf("expected 1 auto generated income transaction for second-hand recovery, got %d", incomeCount)
		}

		// 3.4 再次检查资产大盘：在用固资中该设备已出表，回血总额记录在 sold_recovered_value
		w = httptest.NewRecorder()
		req, _ = http.NewRequest(http.MethodGet, "/api/v1/assets/overview", nil)
		req.Header.Set("Authorization", "Bearer "+testMasterKey)
		r.ServeHTTP(w, req)
		var overviewAfterSell map[string]interface{}
		_ = json.Unmarshal(w.Body.Bytes(), &overviewAfterSell)
		summaryAfterSell := overviewAfterSell["data"].(map[string]interface{})["gear_summary"].(map[string]interface{})
		if int64(summaryAfterSell["sold_recovered_value"].(float64)) < 1250000 {
			t.Fatalf("expected sold_recovered_value >= 1250000, got %v", summaryAfterSell["sold_recovered_value"])
		}

		// 3.5 删除设备单品
		w = httptest.NewRecorder()
		req, _ = http.NewRequest(http.MethodDelete, fmt.Sprintf("/api/v1/assets/gear/%s", gearID), nil)
		req.Header.Set("Authorization", "Bearer "+testMasterKey)
		r.ServeHTTP(w, req)
		if w.Code != http.StatusOK {
			t.Fatalf("expected 200 on delete gear, got %d", w.Code)
		}
	})

	// 4. 记账大件反向同步建入设备库与分期付款支持 (RC-53)
	t.Run("Create Transaction with Installment and Link Gear", func(t *testing.T) {
		txReq := TransactionRequest{
			Amount:                 239900,
			Type:                   "expense",
			Category:               "数码器材",
			Title:                  "AirPods Max 银色",
			Note:                   "头戴式主动降噪耳机",
			PaymentMethod:          "花呗分期",
			IsInstallment:          boolPtr(true),
			InstallmentChannelCode: "huabei",
			InstallmentTotalAmount: 239900,
			InstallmentMonths:      12,
			RepaymentDay:           10,
			LinkGearAsset:          true,
			GearCategory:           "数码器材",
			GearSpec:               "Lightning 接口版",
		}
		body, _ := json.Marshal(txReq)
		w := httptest.NewRecorder()
		req, _ := http.NewRequest(http.MethodPost, "/api/v1/transactions", bytes.NewBuffer(body))
		req.Header.Set("Authorization", "Bearer "+testMasterKey)
		req.Header.Set("Content-Type", "application/json")
		r.ServeHTTP(w, req)
		if w.Code != http.StatusOK {
			t.Fatalf("expected 200 on create transaction with installment, got %d", w.Code)
		}

		// 验证随身设备库自动收录了该条目
		var profile models.CuratorProfile
		db.DB.First(&profile)
		gears := loadProfileGear(&profile)
		foundGear := false
		for _, g := range gears {
			if g.Name == "AirPods Max 银色" && g.PriceCents == 239900 {
				foundGear = true
				break
			}
		}
		if !foundGear {
			t.Fatalf("expected AirPods Max to be linked to EDC gear, but not found")
		}

		// 验证关联心愿单时自动闭环流转为 fulfilled (RG-14)
		wishItem := models.Wish{Title: "购买头戴降噪耳机", Kind: "item", Budget: 250000, Status: "wanting"}
		db.DB.Create(&wishItem)

		txWishReq := TransactionRequest{
			Amount:        239900,
			Type:          "expense",
			Category:      "数码器材",
			Title:         "AirPods Max 银色",
			PaymentMethod: "花呗",
			WishID:        &wishItem.ID,
		}
		bodyWish, _ := json.Marshal(txWishReq)
		w = httptest.NewRecorder()
		req, _ = http.NewRequest(http.MethodPost, "/api/v1/transactions", bytes.NewBuffer(bodyWish))
		req.Header.Set("Authorization", "Bearer "+testMasterKey)
		req.Header.Set("Content-Type", "application/json")
		r.ServeHTTP(w, req)
		if w.Code != http.StatusOK {
			t.Fatalf("expected 200 on create transaction with wish_id, got %d", w.Code)
		}

		var checkWish models.Wish
		db.DB.First(&checkWish, wishItem.ID)
		if checkWish.Status != "fulfilled" || checkWish.TransactionID == nil {
			t.Fatalf("expected wish to be auto-fulfilled via wish_id linkage, got status: %s", checkWish.Status)
		}
	})

	// 5. 三维心愿单四情景归册测试 (RC-53 4 Scenarios)
	t.Run("Wish Resolution Four Scenarios", func(t *testing.T) {
		// 5.1 情景 1: 原配原价达成 (exact)
		wish1 := models.Wish{Title: "Sony 24-70 GM2", Kind: "item", Budget: 1599900, Status: "wanting"}
		db.DB.Create(&wish1)

		resReq1 := ResolveWishRequest{
			ResolutionType:    "exact",
			ActualCost:        1599900,
			CreateTransaction: true,
		}
		body, _ := json.Marshal(resReq1)
		w := httptest.NewRecorder()
		req, _ := http.NewRequest(http.MethodPost, fmt.Sprintf("/api/v1/wishes/%d/resolve", wish1.ID), bytes.NewBuffer(body))
		req.Header.Set("Content-Type", "application/json")
		r.ServeHTTP(w, req)
		if w.Code != http.StatusOK {
			t.Fatalf("expected 200 on exact wish resolve, got %d", w.Code)
		}
		var updatedWish1 models.Wish
		db.DB.First(&updatedWish1, wish1.ID)
		if updatedWish1.Status != "fulfilled" || updatedWish1.ResolutionType != "exact" || updatedWish1.ActualCost != 1599900 {
			t.Fatalf("wish 1 exact resolution failed: %+v", updatedWish1)
		}

		// 5.2 情景 2: 改选替代方案 (altered)
		wish2 := models.Wish{Title: "MacBook Pro 16", Kind: "item", Budget: 1999900, Status: "wanting"}
		db.DB.Create(&wish2)

		resReq2 := ResolveWishRequest{
			ResolutionType:    "altered",
			ActualTitle:       "MacBook Pro 14 M3 Pro",
			ActualCost:        1499900,
			CreateTransaction: true,
		}
		body, _ = json.Marshal(resReq2)
		w = httptest.NewRecorder()
		req, _ = http.NewRequest(http.MethodPost, fmt.Sprintf("/api/v1/wishes/%d/resolve", wish2.ID), bytes.NewBuffer(body))
		req.Header.Set("Content-Type", "application/json")
		r.ServeHTTP(w, req)
		if w.Code != http.StatusOK {
			t.Fatalf("expected 200 on altered wish resolve, got %d", w.Code)
		}
		var updatedWish2 models.Wish
		db.DB.First(&updatedWish2, wish2.ID)
		if updatedWish2.Status != "fulfilled" || updatedWish2.ResolutionType != "altered" || updatedWish2.ActualTitle != "MacBook Pro 14 M3 Pro" {
			t.Fatalf("wish 2 altered resolution failed: %+v", updatedWish2)
		}

		// 5.3 情景 3: 理性拔草 / 释怀放弃 (relinquished)
		wish3 := models.Wish{Title: "劳力士黑水鬼", Kind: "item", Budget: 8000000, Status: "wanting"}
		db.DB.Create(&wish3)

		resReq3 := ResolveWishRequest{
			ResolutionType:  "relinquished",
			ActualCost:      8000000, // 即使前端传了非 0，后端也必须强制归零
			CreateLifeEntry: true,
			EntryContent:    "审视内心后决定放下物欲，心无旁骛方显从容。",
		}
		body, _ = json.Marshal(resReq3)
		w = httptest.NewRecorder()
		req, _ = http.NewRequest(http.MethodPost, fmt.Sprintf("/api/v1/wishes/%d/resolve", wish3.ID), bytes.NewBuffer(body))
		req.Header.Set("Content-Type", "application/json")
		r.ServeHTTP(w, req)
		if w.Code != http.StatusOK {
			t.Fatalf("expected 200 on relinquished wish resolve, got %d", w.Code)
		}
		var updatedWish3 models.Wish
		db.DB.First(&updatedWish3, wish3.ID)
		if updatedWish3.Status != "relinquished" || updatedWish3.ActualCost != 0 || updatedWish3.TransactionID != nil || updatedWish3.GearID != nil {
			t.Fatalf("relinquished wish must have actual_cost=0 and no tx/gear, got: %+v", updatedWish3)
		}

		// 5.4 情景 4: 心愿达成 + 分期信贷 + 一键建入设备库 (RC-53)
		wish4 := models.Wish{Title: "iPad Pro 11 M4", Kind: "item", Spec: "256GB 黑色", Budget: 899900, Status: "wanting"}
		db.DB.Create(&wish4)

		resReq4 := ResolveWishRequest{
			ResolutionType:         "exact",
			ActualCost:             899900,
			CreateTransaction:      true,
			CreateGearAsset:        true,
			IsInstallment:          true,
			InstallmentChannelCode: "baitiao",
			InstallmentMonths:      12,
			RepaymentDay:           20,
		}
		body, _ = json.Marshal(resReq4)
		w = httptest.NewRecorder()
		req, _ = http.NewRequest(http.MethodPost, fmt.Sprintf("/api/v1/wishes/%d/resolve", wish4.ID), bytes.NewBuffer(body))
		req.Header.Set("Content-Type", "application/json")
		r.ServeHTTP(w, req)
		if w.Code != http.StatusOK {
			t.Fatalf("expected 200 on installment wish resolve, got %d", w.Code)
		}
		var updatedWish4 models.Wish
		db.DB.First(&updatedWish4, wish4.ID)
		if updatedWish4.GearID == nil || *updatedWish4.GearID == "" {
			t.Fatalf("expected wish4 to have gear_id linked, got nil")
		}
		if updatedWish4.TransactionID == nil {
			t.Fatalf("expected wish4 to have transaction_id linked, got nil")
		}
		var wish4Tx models.Transaction
		db.DB.First(&wish4Tx, *updatedWish4.TransactionID)
		if !wish4Tx.IsInstallment || wish4Tx.InstallmentChannelCode != "baitiao" || wish4Tx.RepaymentDay != 20 {
			t.Fatalf("wish4 transaction installment data incorrect: %+v", wish4Tx)
		}
	})

	// 6. 分期负债按购买时间 (OccurredAt) 与每月还款日核算，且消除整除尾差
	t.Run("Installment Liability Calculated By Purchase Date Without Rounding Drift", func(t *testing.T) {
		loc := time.Local
		purchaseTime := time.Date(2026, 6, 2, 0, 0, 0, 0, loc)
		checkTime := time.Date(2026, 10, 12, 11, 15, 0, 0, loc)

		// 6月2日购买，每月25日还款，截至10月12日已过 6/25, 7/25, 8/25, 9/25 共 4 期，剩余 8 期
		paid, remain := CalculateElapsedInstallmentMonths(purchaseTime, 12, 25, checkTime)
		if paid != 4 || remain != 8 {
			t.Fatalf("expected paid=4, remain=8 for 2026-06-02 to 2026-10-12 (day 25), got paid=%d, remain=%d", paid, remain)
		}

		// 总额 916900 分 (¥9,169.00)，12期每期 76408 分：
		// 0 期已还时，待还必须精确等于 916900 分（杜绝 76408*12=916896 导致少 4 分尾差）
		paid0, debt0 := CalculateInstallmentAmounts(916900, 12, 76408, 0, 12)
		if paid0 != 0 || debt0 != 916900 {
			t.Fatalf("expected paid0=0, debt0=916900, got paid0=%d, debt0=%d", paid0, debt0)
		}

		// 4 期已还时，已还 = 4*76408 = 305632 分，剩余待还 = 916900 - 305632 = 611268 分 (¥6,112.68)
		paid4, debt4 := CalculateInstallmentAmounts(916900, 12, 76408, 4, 8)
		if paid4 != 305632 || debt4 != 611268 || (paid4+debt4) != 916900 {
			t.Fatalf("expected paid4=305632, debt4=611268, got paid4=%d, debt4=%d", paid4, debt4)
		}
	})
}

// boolPtr 便于在结构体字面量里构造 *bool
func boolPtr(v bool) *bool { return &v }
