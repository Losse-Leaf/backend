package handlers

import (
	"bytes"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"loose-leaf/internal/config"
	"loose-leaf/internal/db"
	"loose-leaf/internal/models"
)

// 验证：年鉴统计排除 installment_repay 本金重复计入，仅累计手续费、阻止修改还款流水 type、
// 并保证 GetTransactions 始终返回 +08:00 北京时间偏移。
func TestAlmanacSummaryExcludesRepayPrincipalAndTransactionsUseBeijingTZ(t *testing.T) {
	setupInstallmentTestDB(t)
	token := "token_test_ledger_audit_reg_123"
	seedTestAdminDevice(token)

	r := setupInstallmentRouter(token)
	r.GET("/api/v1/almanac", GetAnnualAlmanacSummary)

	beijingNov1 := time.Date(2026, 11, 1, 1, 0, 0, 0, config.BeijingLocation)
	txPurchase := models.Transaction{
		Amount:        120000,
		Type:          "expense",
		Category:      "数码器材",
		Title:         "耳机分期购买",
		OccurredAt:    beijingNov1,
		IsInstallment: true,
		Source:        "installment_purchase",
	}
	if err := db.DB.Create(&txPurchase).Error; err != nil {
		t.Fatalf("create purchase tx: %v", err)
	}

	txRepay := models.Transaction{
		Amount:          40600,
		Type:            "expense",
		Category:        "利息手续费",
		Title:           "耳机分期 · 第 1/3 期",
		OccurredAt:      beijingNov1.Add(2 * time.Hour),
		Source:          "installment_repay",
		PrincipalAmount: 40000,
		FeeAmount:       600,
	}
	if err := db.DB.Create(&txRepay).Error; err != nil {
		t.Fatalf("create repay tx: %v", err)
	}

	// 通过 POST 接口写入一笔 UTC 2026-10-31T17:00:00Z（即北京时间 2026-11-01T01:00:00+08:00）的收入
	createIncomeBody, _ := json.Marshal(map[string]any{
		"amount":      500000,
		"type":        "income",
		"category":    "工资收入",
		"title":       "工资",
		"occurred_at": "2026-10-31T17:00:00Z",
	})
	reqCreate, _ := http.NewRequest("POST", "/api/v1/transactions", bytes.NewBuffer(createIncomeBody))
	reqCreate.Header.Set("Authorization", "Bearer "+token)
	reqCreate.Header.Set("Content-Type", "application/json")
	wCreate := httptest.NewRecorder()
	r.ServeHTTP(wCreate, reqCreate)
	if wCreate.Code != http.StatusOK {
		t.Fatalf("create income tx failed: code=%d body=%s", wCreate.Code, wCreate.Body.String())
	}

	// 1. 验证 /api/v1/almanac?year=2026 口径：支出 = 120000 + 600 = 120600，收入 = 500000，结余 = 379400
	reqAlm, _ := http.NewRequest("GET", "/api/v1/almanac?year=2026", nil)
	reqAlm.Header.Set("Authorization", "Bearer "+token)
	wAlm := httptest.NewRecorder()
	r.ServeHTTP(wAlm, reqAlm)
	if wAlm.Code != http.StatusOK {
		t.Fatalf("almanac status=%d body=%s", wAlm.Code, wAlm.Body.String())
	}
	var almResp struct {
		Data struct {
			Finances struct {
				TotalIncome  int64 `json:"total_income"`
				TotalExpense int64 `json:"total_expense"`
				Balance      int64 `json:"balance"`
			} `json:"finances"`
		} `json:"data"`
	}
	if err := json.Unmarshal(wAlm.Body.Bytes(), &almResp); err != nil {
		t.Fatalf("unmarshal almanac: %v", err)
	}
	if almResp.Data.Finances.TotalIncome != 500000 {
		t.Errorf("almanac total_income want 500000, got %d", almResp.Data.Finances.TotalIncome)
	}
	if almResp.Data.Finances.TotalExpense != 120600 {
		t.Errorf("almanac total_expense want 120600 (excluding repay principal), got %d", almResp.Data.Finances.TotalExpense)
	}
	if almResp.Data.Finances.Balance != 379400 {
		t.Errorf("almanac balance want 379400, got %d", almResp.Data.Finances.Balance)
	}

	// 2. 验证禁止将 installment_repay 改类型为 income
	updBody, _ := json.Marshal(map[string]any{
		"amount":   40600,
		"type":     "income",
		"category": "工资收入",
	})
	reqUpd, _ := http.NewRequest("PUT", fmt.Sprintf("/api/v1/transactions/%d", txRepay.ID), bytes.NewBuffer(updBody))
	reqUpd.Header.Set("Authorization", "Bearer "+token)
	reqUpd.Header.Set("Content-Type", "application/json")
	wUpd := httptest.NewRecorder()
	r.ServeHTTP(wUpd, reqUpd)
	if wUpd.Code != http.StatusBadRequest {
		t.Errorf("updating installment_repay type to income should return 400, got %d (%s)", wUpd.Code, wUpd.Body.String())
	}

	// 3. 验证 GetTransactions 输出的 occurred_at 始终带 +08:00 偏移（不按 UTC Z 输出）
	reqList, _ := http.NewRequest("GET", "/api/v1/transactions?year=2026&month=11", nil)
	reqList.Header.Set("Authorization", "Bearer "+token)
	wList := httptest.NewRecorder()
	r.ServeHTTP(wList, reqList)
	if wList.Code != http.StatusOK {
		t.Fatalf("list status=%d body=%s", wList.Code, wList.Body.String())
	}
	var listResp struct {
		Data []struct {
			ID         uint   `json:"id"`
			OccurredAt string `json:"occurred_at"`
		} `json:"data"`
	}
	if err := json.Unmarshal(wList.Body.Bytes(), &listResp); err != nil {
		t.Fatalf("unmarshal list: %v", err)
	}
	if len(listResp.Data) != 3 {
		t.Fatalf("expected 3 transactions in Nov 2026 Beijing time, got %d", len(listResp.Data))
	}
	for _, row := range listResp.Data {
		if !strings.HasSuffix(row.OccurredAt, "+08:00") {
			t.Errorf("tx %d occurred_at should end with +08:00, got %q", row.ID, row.OccurredAt)
		}
		if !strings.HasPrefix(row.OccurredAt, "2026-11-01") {
			t.Errorf("tx %d UTC 2026-10-31T17:00Z should format as 2026-11-01 in Beijing time, got %q", row.ID, row.OccurredAt)
		}
	}
}
