package handlers

import (
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"loose-leaf/internal/db"
	"loose-leaf/internal/models"

	"github.com/gin-gonic/gin"
)

// 旅程花销的统计口径（RG / 旅途花销容差收紧）
//
// 关键：原先口径是「显式挂载 ∪ 同城 ±1 天窗口内的未挂载账目」，
// 于是**同城的无关开销**会被算进旅程花销 ——
// 旅程前一天在本地买的东西、旅程当天没出门却记的账，全都被算上，
// 旅程卡片上的「这趟花了多少」于是**虚高**。
//
// 现在口径收紧为「**只统计显式挂载**的账目」，
// 模糊匹配的职责回归「把该挂的挂上」（linkUnlinkedTransactionsToFootprint），
// 历史数据靠**挂载**继续计入，而不是靠模糊匹配直接计入。
func TestFootprintExpenseAttribution(t *testing.T) {
	gin.SetMode(gin.TestMode)
	setupTestFullDB(t)

	visited := time.Date(2026, 10, 10, 10, 0, 0, 0, time.Local)
	fp := models.Footprint{
		CityCode:  "110100",
		CityName:  "北京市",
		Title:     "北京出差",
		VisitedAt: visited,
	}
	if err := db.DB.Create(&fp).Error; err != nil {
		t.Fatalf("create footprint: %v", err)
	}

	mk := func(title string, amount int64, cityCode string, occurredAt time.Time, footprintID *uint) models.Transaction {
		tx := models.Transaction{
			Amount:      amount,
			Type:        "expense",
			Category:    "数码器材",
			Title:       title,
			OccurredAt:  occurredAt,
			CityCode:    cityCode,
			FootprintID: footprintID,
		}
		if err := db.DB.Create(&tx).Error; err != nil {
			t.Fatalf("create transaction: %v", err)
		}
		return tx
	}

	// 1. 显式挂载的账目 —— 必须计入
	fpID := fp.ID
	linked := mk("行程内的显示器", 120000, "110100", visited, &fpID)

	// 2. 同城、旅程前一天、**未挂载**的账目 —— 原来会被误算进去
	mk("旅程前一天的本地采购", 50000, "110100", visited.AddDate(0, 0, -1), nil)

	// 3. 其它城市的账目 —— 本来就不该算
	mk("上海的差旅", 30000, "310100", visited, nil)

	var stats struct {
		TotalSpent       int64
		TransactionCount int64
	}
	db.DB.Model(&models.Transaction{}).
		Select("COALESCE(SUM(amount), 0) AS total_spent, COUNT(*) AS transaction_count").
		Where("type = 'expense' AND footprint_id = ?", fp.ID).
		Scan(&stats)

	if stats.TotalSpent != linked.Amount {
		t.Errorf("旅程花销应只统计显式挂载的账目：期望 %d，实际 %d（同城未挂载的账目不得被算进去）",
			linked.Amount, stats.TotalSpent)
	}
	if stats.TransactionCount != 1 {
		t.Errorf("旅程花销笔数应为 1，实际 %d", stats.TransactionCount)
	}

	// 4. 模糊匹配的职责回归「挂载」：回填后那笔同城账目**应当被挂上**，
	//    挂上之后它才会计入 —— 模糊匹配没有被丢掉，只是不再直接计入。
	//
	//    注意：回填与自动挂载共用**同一套**规则（同城 + 旅程窗口 ± 容差 + 取最近的一趟），
	//    否则「老数据按一套口径挂、新数据按另一套口径挂」，旅程花销会忽高忽低。
	linkedCount := linkUnlinkedTransactions("")
	if linkedCount != 1 {
		t.Fatalf("回填应挂上 1 笔（同城且在旅程窗口内的未挂载账目），实际 %d", linkedCount)
	}
	var afterTx models.Transaction
	if err := db.DB.Where("title = ?", "旅程前一天的本地采购").First(&afterTx).Error; err != nil {
		t.Fatalf("回填后查不到该账目: %v", err)
	}
	if afterTx.FootprintID == nil || *afterTx.FootprintID != fp.ID {
		t.Errorf("回填后该账目应当被挂到旅程上（否则口径收紧后它就永远不计入了），实际 footprint_id=%v", afterTx.FootprintID)
	}

	// 5. 其它城市的账目**不得**被挂上
	var otherTx models.Transaction
	if err := db.DB.Where("title = ?", "上海的差旅").First(&otherTx).Error; err != nil {
		t.Fatalf("查不到该账目: %v", err)
	}
	if otherTx.FootprintID != nil && *otherTx.FootprintID != 0 {
		t.Errorf("其它城市的账目不得被挂到本旅程上，实际 footprint_id=%v", *otherTx.FootprintID)
	}
}

// 启动回填是幂等的：重复跑不应重复挂载，也不应报错
func TestEnsureFootprintTransactionsLinkedIsIdempotent(t *testing.T) {
	gin.SetMode(gin.TestMode)
	setupTestFullDB(t)

	visited := time.Date(2026, 10, 10, 10, 0, 0, 0, time.Local)
	fp := models.Footprint{CityCode: "110100", CityName: "北京市", Title: "北京出差", VisitedAt: visited}
	if err := db.DB.Create(&fp).Error; err != nil {
		t.Fatalf("create footprint: %v", err)
	}
	tx := models.Transaction{
		Amount: 10000, Type: "expense", Category: "餐饮", Title: "行程午餐",
		OccurredAt: visited, CityCode: "110100",
	}
	if err := db.DB.Create(&tx).Error; err != nil {
		t.Fatalf("create transaction: %v", err)
	}

	EnsureFootprintTransactionsLinked()
	EnsureFootprintTransactionsLinked()

	var count int64
	db.DB.Model(&models.Transaction{}).Where("footprint_id = ?", fp.ID).Count(&count)
	if count != 1 {
		t.Errorf("回填后应恰好挂上 1 笔，实际 %d（回填必须幂等）", count)
	}
}

// 旅程的「全部历史窗口」不应再影响花销统计（容差已去除）
func TestFootprintExpenseIgnoresAdjacentDays(t *testing.T) {
	gin.SetMode(gin.TestMode)
	setupTestFullDB(t)

	visited := time.Date(2026, 10, 10, 10, 0, 0, 0, time.Local)
	fp := models.Footprint{CityCode: "110100", CityName: "北京市", Title: "北京出差", VisitedAt: visited}
	if err := db.DB.Create(&fp).Error; err != nil {
		t.Fatalf("create footprint: %v", err)
	}
	// 同城、旅程**前一天**的未挂载账目
	tx := models.Transaction{
		Amount: 77000, Type: "expense", Category: "购物", Title: "前一天本地买的东西",
		OccurredAt: visited.AddDate(0, 0, -1), CityCode: "110100",
	}
	if err := db.DB.Create(&tx).Error; err != nil {
		t.Fatalf("create transaction: %v", err)
	}

	populateFootprintExpenseStats(&fp, true)
	if fp.TotalSpent != 0 {
		t.Errorf("同城、旅程前一天的未挂载账目不得计入旅程花销（±1 天容差已去除），实际 %d", fp.TotalSpent)
	}
	if fp.TransactionCount != 0 {
		t.Errorf("笔数同样应为 0，实际 %d", fp.TransactionCount)
	}
}

// 供路由测试复用：确认统计接口本身仍然可用
func TestFootprintExpenseStatsEndpointShape(t *testing.T) {
	gin.SetMode(gin.TestMode)
	setupTestFullDB(t)
	r := gin.New()
	r.GET("/api/v1/footprints/:id", GetFootprintByID)

	visited := time.Date(2026, 10, 10, 10, 0, 0, 0, time.Local)
	fp := models.Footprint{CityCode: "110100", CityName: "北京市", Title: "北京出差", VisitedAt: visited}
	if err := db.DB.Create(&fp).Error; err != nil {
		t.Fatalf("create footprint: %v", err)
	}
	fpID := fp.ID
	tx := models.Transaction{
		Amount: 25000, Type: "expense", Category: "交通", Title: "高铁票",
		OccurredAt: visited, CityCode: "110100", FootprintID: &fpID,
	}
	if err := db.DB.Create(&tx).Error; err != nil {
		t.Fatalf("create transaction: %v", err)
	}

	req := httptest.NewRequest(http.MethodGet, "/api/v1/footprints/1", nil)
	w := httptest.NewRecorder()
	r.ServeHTTP(w, req)
	if w.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d: %s", w.Code, w.Body.String())
	}
}
