package handlers

import (
	"log"
	"net/http"
	"regexp"
	"strings"
	"unicode"

	"github.com/gin-gonic/gin"
	"gorm.io/gorm"
	"loose-leaf/internal/db"
	"loose-leaf/internal/models"
)

var (
	ruleBranchSuffixRegex = regexp.MustCompile(`(?:\s*[（(][^（）()]{1,16}(?:店|分店|广场|校区|中心|站|支行|网点)[)）])\s*$`)
	ruleWhitespaceRegex   = regexp.MustCompile(`\s+`)
)

var genericMerchantPlaceholders = map[string]bool{
	"自动记账":    true,
	"微信支付自动记账": true,
	"支付宝自动记账":  true,
	"银行卡自动记账":  true,
	"微信转账":    true,
	"支付宝转账":   true,
	"微信转账收款":  true,
	"收到款项":    true,
	"消费":      true,
	"入账":      true,
	"收款":      true,
	"付款":      true,
	"转账":      true,
}

var genericMerchantPrefixes = []string{
	"转账 · ",
	"收到转账 · ",
	"收到款项 · ",
	"财付通-",
	"财付通—",
	"支付宝-",
	"支付宝—",
	"微信支付-",
	"微信支付—",
}

// NormalizeMerchantRuleKey 将交易标题/商户名清洗为稳定的归类记忆键与展示名。
// 返回空串表示该标题为通用占位文案、纯数字或过短，不适合作为自动归类记忆。
func NormalizeMerchantRuleKey(rawTitle string) (matchKey string, displayName string) {
	cleaned := strings.TrimSpace(rawTitle)
	if cleaned == "" {
		return "", ""
	}
	if strings.HasPrefix(cleaned, "自动记账:") || strings.HasPrefix(cleaned, "自动记账：") ||
		strings.HasSuffix(cleaned, " · 消费") || strings.HasSuffix(cleaned, " · 入账") {
		return "", ""
	}

	for {
		stripped := false
		for _, prefix := range genericMerchantPrefixes {
			if strings.HasPrefix(cleaned, prefix) {
				cleaned = strings.TrimSpace(strings.TrimPrefix(cleaned, prefix))
				stripped = true
			}
		}
		if !stripped {
			break
		}
	}

	cleaned = ruleBranchSuffixRegex.ReplaceAllString(cleaned, "")
	cleaned = strings.TrimSpace(ruleWhitespaceRegex.ReplaceAllString(cleaned, " "))
	if cleaned == "" || genericMerchantPlaceholders[cleaned] {
		return "", ""
	}

	if len([]rune(cleaned)) < 2 {
		return "", ""
	}

	hasNonDigitOrPunct := false
	for _, r := range cleaned {
		if !unicode.IsDigit(r) && !unicode.IsSpace(r) && !unicode.IsPunct(r) && !unicode.IsSymbol(r) {
			hasNonDigitOrPunct = true
			break
		}
	}
	if !hasNonDigitOrPunct {
		return "", ""
	}

	runes := []rune(cleaned)
	if len(runes) > 120 {
		cleaned = string(runes[:120])
	}
	matchKey = strings.ToLower(cleaned)
	return matchKey, cleaned
}

// GetCategoryRules 获取全部自动归类记忆规则
func GetCategoryRules(c *gin.Context) {
	var rules []models.CategoryRule
	if err := db.DB.Order("updated_at DESC, hit_count DESC, id DESC").Find(&rules).Error; err != nil {
		Error(c, http.StatusInternalServerError, "获取自动归类记忆失败")
		return
	}
	Success(c, rules)
}

type UpsertCategoryRuleRequest struct {
	TxType      string `json:"tx_type" binding:"required"`
	MatchKey    string `json:"match_key"`
	DisplayName string `json:"display_name"`
	Category    string `json:"category" binding:"required"`
	Source      string `json:"source"`
}

// UpsertCategoryRule 创建或更新单条商户自动归类规则
func UpsertCategoryRule(c *gin.Context) {
	var req UpsertCategoryRuleRequest
	if err := c.ShouldBindJSON(&req); err != nil {
		Error(c, http.StatusBadRequest, "请填写收支类型、商户关键词与目标分类")
		return
	}

	txType := strings.TrimSpace(req.TxType)
	if txType != "expense" && txType != "income" {
		Error(c, http.StatusBadRequest, "tx_type 仅支持 expense / income")
		return
	}

	category := strings.TrimSpace(req.Category)
	if !transactionCategoryAllowed(txType, category) {
		Error(c, http.StatusBadRequest, "目标分类不存在于分类字典")
		return
	}

	rawCandidate := strings.TrimSpace(req.DisplayName)
	if rawCandidate == "" {
		rawCandidate = strings.TrimSpace(req.MatchKey)
	}
	matchKey, displayName := NormalizeMerchantRuleKey(rawCandidate)
	if matchKey == "" {
		Error(c, http.StatusBadRequest, "商户/关键词过短或为无效占位文案")
		return
	}
	if strings.TrimSpace(req.MatchKey) != "" {
		if normKey, _ := NormalizeMerchantRuleKey(req.MatchKey); normKey != "" {
			matchKey = normKey
		}
	}

	source := strings.TrimSpace(req.Source)
	if source != "manual" && source != "learned" {
		source = "manual"
	}

	rule, err := upsertCategoryRuleRecord(db.DB, txType, matchKey, displayName, category, source)
	if err != nil {
		Error(c, http.StatusInternalServerError, "保存自动归类规则失败")
		return
	}
	Success(c, rule)
}

// DeleteCategoryRule 删除单条商户自动归类规则
func DeleteCategoryRule(c *gin.Context) {
	id := c.Param("id")
	var rule models.CategoryRule
	if err := db.DB.First(&rule, "id = ?", id).Error; err != nil {
		Error(c, http.StatusNotFound, "该归类规则不存在")
		return
	}
	if err := db.DB.Delete(&rule).Error; err != nil {
		Error(c, http.StatusInternalServerError, "删除归类规则失败")
		return
	}
	Success(c, gin.H{"id": rule.ID, "deleted": true})
}

func upsertCategoryRuleRecord(txDB *gorm.DB, txType, matchKey, displayName, category, source string) (*models.CategoryRule, error) {
	var existing models.CategoryRule
	err := txDB.Where("tx_type = ? AND match_key = ?", txType, matchKey).First(&existing).Error
	if err == nil && existing.ID > 0 {
		existing.Category = category
		existing.DisplayName = displayName
		existing.Source = source
		existing.HitCount++
		if err := txDB.Save(&existing).Error; err != nil {
			return nil, err
		}
		return &existing, nil
	}

	rule := models.CategoryRule{
		TxType:      txType,
		MatchKey:    matchKey,
		DisplayName: displayName,
		Category:    category,
		HitCount:    1,
		Source:      source,
	}
	if err := txDB.Create(&rule).Error; err != nil {
		return nil, err
	}
	return &rule, nil
}

// LearnMerchantCategoryRule 在用户修改交易分类后静默记录商户映射（失败仅记录日志，不阻断主流程）
func LearnMerchantCategoryRule(txDB *gorm.DB, txType, title, category string) {
	if txDB == nil || !txDB.Migrator().HasTable(&models.CategoryRule{}) {
		return
	}
	if txType != "expense" && txType != "income" {
		return
	}
	cat := strings.TrimSpace(category)
	if cat == "" || cat == "其他" || cat == "其他收入" {
		return
	}
	matchKey, displayName := NormalizeMerchantRuleKey(title)
	if matchKey == "" || strings.EqualFold(matchKey, cat) || strings.EqualFold(displayName, cat) {
		return
	}
	if _, err := upsertCategoryRuleRecord(txDB, txType, matchKey, displayName, cat, "learned"); err != nil {
		log.Printf("LearnMerchantCategoryRule failed: %v", err)
	}
}
