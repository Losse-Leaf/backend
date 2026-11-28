package db

import (
	"log"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"loose-leaf/internal/config"
	"loose-leaf/internal/models"

	"github.com/glebarez/sqlite"
	"gorm.io/gorm"
	"gorm.io/gorm/logger"
)

var DB *gorm.DB

// lifecycleMu 保护 DB 连接池与磁盘文件的「代际」不被整库恢复打断。
//
// 约定：
//   - 任何会使用 DB 的请求或后台任务，在操作期间持读锁；
//   - 整库恢复持写锁，覆盖关闭旧池、替换文件、重连与解压媒体；
//   - 锁序固定为 lifecycle → backupMutex，禁止反向；
//   - 读锁不可重入：已持读锁的代码不得再调用 LockRead（写锁排队时会死锁）。
var lifecycleMu sync.RWMutex

// LockRead 获取数据库生命周期读锁
func LockRead() { lifecycleMu.RLock() }

// UnlockRead 释放数据库生命周期读锁
func UnlockRead() { lifecycleMu.RUnlock() }

// LockWrite 获取数据库生命周期写锁（仅整库恢复使用）
func LockWrite() { lifecycleMu.Lock() }

// UnlockWrite 释放数据库生命周期写锁
func UnlockWrite() { lifecycleMu.Unlock() }

// InitDB 初始化 SQLite 数据库
func InitDB(dbPath string) (*gorm.DB, error) {
	if dbPath == "" {
		dbPath = config.DefaultDBPath
	}

	dir := filepath.Dir(dbPath)
	// 关键：目录 0700 —— 数据库里存着 R2 桶的密钥与主理人口令哈希，
	// 0755 意味着**同机的其它用户可以读到**。
	if err := os.MkdirAll(dir, 0700); err != nil {
		return nil, err
	}
	// 已存在的目录也收紧一次（老部署可能是 0755）
	_ = os.Chmod(dir, 0700)

	// 启用 WAL 模式、5 秒忙碌重试与普通同步模式，避免并发写锁死。
	// 注意：glebarez/go-sqlite 只认 `_pragma` 查询参数，
	// 早期写的 `_journal_mode=WAL&_busy_timeout=...` 会被驱动**静默忽略** ——
	// 库实际一直跑在 rollback journal 模式下，与 WAL 相关的恢复/并发假设都不成立。
	dsn := dbPath
	if !strings.Contains(dsn, "?") {
		dsn += "?_pragma=journal_mode(WAL)&_pragma=busy_timeout(5000)&_pragma=synchronous(NORMAL)"
	}

	database, err := gorm.Open(sqlite.Open(dsn), &gorm.Config{
		Logger:                                   logger.Default.LogMode(logger.Warn),
		DisableForeignKeyConstraintWhenMigrating: true,
	})
	if err != nil {
		return nil, err
	}

	// 数据库文件本身也收紧到 0600（SQLite 驱动按 umask 落地，可能是 0644）。
	// 关键（发现 21）：必须在 gorm.Open（真正创建数据库文件）**之后**执行 ——
	// 早先放在 Open 之前，首次建库时文件尚不存在，chmod 空打，
	// 新库仍按 umask 落地，收紧从未对「首次创建」生效过。
	_ = os.Chmod(dbPath, 0600)

	// 调优连接池：SQLite 启用 WAL 模式后支持并发读取。配合 _busy_timeout=5000 适当放宽连接池并发度 (BE-07)
	if sqlDB, err := database.DB(); err == nil {
		sqlDB.SetMaxOpenConns(10)
		sqlDB.SetMaxIdleConns(5)
		sqlDB.SetConnMaxLifetime(time.Hour)
	}

	// 自动迁移表结构
	err = database.AutoMigrate(
		&models.LifeEntry{},
		&models.Attachment{},
		&models.Transaction{},
		&models.CreditChannel{},
		&models.InstallmentPlan{},
		&models.InstallmentItem{},
		&models.CategoryOption{},
		&models.CategoryRule{},
		&models.SiteConfig{},
		&models.Device{},
		&models.PairTicket{},
		&models.CuratorProfile{},
		&models.Wish{},
		&models.SyncTombstone{},
	)
	if err != nil {
		return nil, err
	}

	DB = database
	_ = database.Exec("DROP INDEX IF EXISTS idx_pair_tickets_pin_code;").Error
	_ = database.Exec("CREATE INDEX IF NOT EXISTS idx_att_refs ON attachments(entry_id, wish_id);").Error
	_ = database.Exec("CREATE INDEX IF NOT EXISTS idx_tx_type_occurred ON transactions(type, occurred_at DESC);").Error
	_ = database.Exec("CREATE INDEX IF NOT EXISTS idx_wish_status_kind_created ON wishes(status, kind, created_at DESC);").Error
	// 历史器物伴生散页曾使用契约外的 type="gear"，统一归入 life 并推进 updated_at。
	_ = database.Exec("UPDATE life_entries SET type = 'life', updated_at = ? WHERE type = 'gear'", config.NowInBeijing()).Error
	// Web 端「信用卡 / 自定义分期」历史上直接发 installment_channel_code='custom'
	// 升级为真实渠道行后 custom 与普通渠道同规则：必须存在、还款日单源化。
	EnsureCustomInstallmentChannel(database)
	// 「好物购置」为心愿归册自动账单的生成分类（wishes.go ResolveWish），
	// 存量库 targeted 补种（SeedDefaultCategories 只补空 scope），
	// 否则分类字典收口后历史心愿账目在编辑时会被 400。
	var goodBuyCount int64
	database.Model(&models.CategoryOption{}).Where("scope = ? AND name = ?", "expense", "好物购置").Count(&goodBuyCount)
	if goodBuyCount == 0 {
		database.Create(&models.CategoryOption{Scope: "expense", Name: "好物购置", GroupName: "", SortOrder: 900, IsBuiltin: true})
	}
	// 「利息手续费」为分期还款手续费的内置分类 (RC-140)
	var feeCount int64
	database.Model(&models.CategoryOption{}).Where("scope = ? AND name = ?", "expense", "利息手续费").Count(&feeCount)
	if feeCount == 0 {
		database.Create(&models.CategoryOption{Scope: "expense", Name: "利息手续费", GroupName: "", SortOrder: 910, IsBuiltin: true})
	}
	seedData(database)
	return database, nil
}

// EnsureCustomInstallmentChannel 幂等确保 code='custom' 的真实渠道行存在（幂等启动迁移）。
// 还款日取现存 custom 分期账目中出现最多的还款日（沿用历史口径），无历史账目时兜底 10。
// 行标记为内置：不可删除 —— 删掉会让 Web 端「自定义分期」表单立即 400。
func EnsureCustomInstallmentChannel(database *gorm.DB) {
	var count int64
	database.Model(&models.CreditChannel{}).Where("code = ?", "custom").Count(&count)
	if count > 0 {
		return
	}
	day := 10
	var modes []struct {
		Day int
		N   int64
	}
	database.Model(&models.Transaction{}).
		Select("repayment_day AS day, COUNT(*) AS n").
		Where("installment_channel_code = ? AND repayment_day > 0", "custom").
		Group("repayment_day").Order("n DESC, day ASC").Limit(1).Scan(&modes)
	if len(modes) > 0 && modes[0].Day > 0 {
		day = modes[0].Day
	}
	database.Create(&models.CreditChannel{
		Code:         "custom",
		Name:         "自定义分期",
		RepaymentDay: day,
		IsBuiltin:    true,
		Color:        "var(--accent-warm)",
	})
}

// HiddenSystemExpenseCategories 系统内部自动账单专用分类（不在默认字典/分类管理界面暴露，但内部校验放行且禁止删除）
var HiddenSystemExpenseCategories = map[string]bool{
	"好物购置":  true,
	"利息手续费": true,
}

// HiddenSystemExpenseNames 返回隐藏系统分类切片供 SQL 过滤复用
func HiddenSystemExpenseNames() []string {
	return []string{"好物购置", "利息手续费"}
}

// DefaultCategorySeed 系统预置分类字典单一事实源
type DefaultCategorySeed struct {
	Scope     string
	Name      string
	GroupName string
	SortOrder int
}

// DefaultCategorySeeds 精简 v3 分类字典（全 2 字扁平大类，不再使用二级分组与往来/系统外露）
var DefaultCategorySeeds = []DefaultCategorySeed{
	// 支出：9 项扁平大类
	{Scope: "expense", Name: "餐饮", GroupName: "", SortOrder: 10},
	{Scope: "expense", Name: "交通", GroupName: "", SortOrder: 20},
	{Scope: "expense", Name: "购物", GroupName: "", SortOrder: 30},
	{Scope: "expense", Name: "居家", GroupName: "", SortOrder: 40},
	{Scope: "expense", Name: "订阅", GroupName: "", SortOrder: 50},
	{Scope: "expense", Name: "娱乐", GroupName: "", SortOrder: 60},
	{Scope: "expense", Name: "医疗", GroupName: "", SortOrder: 70},
	{Scope: "expense", Name: "学习", GroupName: "", SortOrder: 80},
	{Scope: "expense", Name: "其他", GroupName: "", SortOrder: 90},

	// 收入：5 项扁平大类
	{Scope: "income", Name: "工资", GroupName: "", SortOrder: 10},
	{Scope: "income", Name: "副业", GroupName: "", SortOrder: 20},
	{Scope: "income", Name: "理财", GroupName: "", SortOrder: 30},
	{Scope: "income", Name: "退款", GroupName: "", SortOrder: 40},
	{Scope: "income", Name: "其他", GroupName: "", SortOrder: 50},

	// 支付方式：6 项
	{Scope: "payment", Name: "微信支付", GroupName: "", SortOrder: 10},
	{Scope: "payment", Name: "支付宝", GroupName: "", SortOrder: 20},
	{Scope: "payment", Name: "银行卡", GroupName: "", SortOrder: 30},
	{Scope: "payment", Name: "信用卡", GroupName: "", SortOrder: 40},
	{Scope: "payment", Name: "现金", GroupName: "", SortOrder: 50},
	{Scope: "payment", Name: "其他", GroupName: "", SortOrder: 60},
}

// DefaultCategoriesMap 系统预置的三大分类字典默认值（由 DefaultCategorySeeds 派生）
var DefaultCategoriesMap = func() map[string][]string {
	out := map[string][]string{
		"expense": {},
		"income":  {},
		"payment": {},
	}
	for _, seed := range DefaultCategorySeeds {
		out[seed.Scope] = append(out[seed.Scope], seed.Name)
	}
	return out
}()

func seedsForScope(scope string) []DefaultCategorySeed {
	var out []DefaultCategorySeed
	for _, seed := range DefaultCategorySeeds {
		if seed.Scope == scope {
			out = append(out, seed)
		}
	}
	return out
}

// SeedDefaultCategories 为空态 scope 补充预置分类字典
func SeedDefaultCategories(database *gorm.DB) {
	scopes := []string{"expense", "income", "payment"}
	for _, sc := range scopes {
		var cnt int64
		q := database.Model(&models.CategoryOption{}).Where("scope = ?", sc)
		if sc == "expense" {
			q = q.Where("name NOT IN ?", HiddenSystemExpenseNames())
		}
		q.Count(&cnt)
		if cnt > 0 {
			continue
		}
		seeds := seedsForScope(sc)
		batch := make([]models.CategoryOption, 0, len(seeds))
		for _, seed := range seeds {
			batch = append(batch, models.CategoryOption{
				Scope:     sc,
				Name:      seed.Name,
				GroupName: "",
				SortOrder: seed.SortOrder,
				IsBuiltin: true,
			})
		}
		if len(batch) > 0 {
			database.Create(&batch)
		}
	}
}

func mergeKeywordStrings(existingRaw, incomingRaw string) string {
	seen := map[string]bool{}
	var out []string
	for _, src := range []string{existingRaw, incomingRaw} {
		for _, part := range strings.Split(strings.NewReplacer("，", ",", "、", ",", ";", ",", "；", ",").Replace(src), ",") {
			kw := strings.TrimSpace(part)
			if kw == "" {
				continue
			}
			low := strings.ToLower(kw)
			if !seen[low] {
				seen[low] = true
				out = append(out, kw)
			}
		}
	}
	return strings.Join(out, ",")
}

// UpgradeCategoryCatalogV3 将历史 v1/v2 分类平滑归并升级为 2 字扁平 v3 大类体系：
// - 迁移旧版细分类流水、分期计划与归类规则到对应 2 字大类；
// - 合并旧内置分类的自定义关键词并清理多余的内置子分类行（绝不删除用户自定义分类）；
// - 清空内置分类二级分组并对齐排序；
// - 通过 SiteConfig.CategoryCatalogVersion >= 3 保证后续不再复活用户手动删除的分类。
func UpgradeCategoryCatalogV3(database *gorm.DB) {
	var cfg models.SiteConfig
	if err := database.Order("id ASC").First(&cfg).Error; err != nil {
		return
	}
	if cfg.CategoryCatalogVersion >= 3 {
		return
	}

	expenseMigrations := map[string]string{
		"咖啡饮品": "餐饮",
		"买菜生鲜": "餐饮",
		"零食水果": "餐饮",
		"旅行度假": "娱乐",
		"运动健康": "娱乐",
		"宠物":   "娱乐",
		"日用百货": "购物",
		"服饰美妆": "购物",
		"数码器材": "购物",
		"住房":   "居家",
		"水电燃气": "居家",
		"通讯网费": "居家",
		"订阅会员": "订阅",
		"人情社交": "其他",
	}
	incomeMigrations := map[string]string{
		"工资收入": "工资",
		"奖金补贴": "工资",
		"兼职副业": "副业",
		"稿酬设计": "副业",
		"投资理财": "理财",
		"二手回血": "退款",
		"退款报销": "退款",
		"礼金红包": "其他",
		"亲友转账": "其他",
		"其他收入": "其他",
	}

	_ = database.Transaction(func(tx *gorm.DB) error {
		scopeCounts := map[string]int64{}
		for _, sc := range []string{"expense", "income", "payment"} {
			var cnt int64
			q := tx.Model(&models.CategoryOption{}).Where("scope = ?", sc)
			if sc == "expense" {
				q = q.Where("name NOT IN ?", HiddenSystemExpenseNames())
			}
			q.Count(&cnt)
			scopeCounts[sc] = cnt
		}

		// 1. 先确保 v3 预置种子存在（仅针对已有字典数据的 scope）
		for _, seed := range DefaultCategorySeeds {
			if scopeCounts[seed.Scope] == 0 {
				continue
			}
			var existing models.CategoryOption
			err := tx.Where("scope = ? AND name = ?", seed.Scope, seed.Name).First(&existing).Error
			if err == nil && existing.ID > 0 {
				updates := map[string]any{}
				if existing.GroupName != "" {
					updates["group_name"] = ""
				}
				if existing.IsBuiltin && existing.SortOrder != seed.SortOrder {
					updates["sort_order"] = seed.SortOrder
				}
				if len(updates) > 0 {
					if err := tx.Model(&existing).Updates(updates).Error; err != nil {
						return err
					}
				}
				continue
			}
			if err := tx.Create(&models.CategoryOption{
				Scope:     seed.Scope,
				Name:      seed.Name,
				GroupName: "",
				SortOrder: seed.SortOrder,
				IsBuiltin: true,
			}).Error; err != nil {
				return err
			}
		}

		// 2. 平滑迁移历史/v2 分类的账目、分期、规则、关键词，并移除退役的内置项
		migrateScope := func(scope string, mapping map[string]string) error {
			for oldName, newName := range mapping {
				if err := tx.Model(&models.Transaction{}).
					Where("type = ? AND category = ?", scope, oldName).
					Update("category", newName).Error; err != nil {
					return err
				}
				if scope == "expense" && tx.Migrator().HasTable(&models.InstallmentPlan{}) {
					if err := tx.Model(&models.InstallmentPlan{}).
						Where("category_name = ?", oldName).
						Update("category_name", newName).Error; err != nil {
						return err
					}
				}
				if tx.Migrator().HasTable(&models.CategoryRule{}) {
					if err := tx.Model(&models.CategoryRule{}).
						Where("tx_type = ? AND category = ?", scope, oldName).
						Update("category", newName).Error; err != nil {
						return err
					}
				}

				var oldOpt models.CategoryOption
				if err := tx.Where("scope = ? AND name = ? AND is_builtin = ?", scope, oldName, true).First(&oldOpt).Error; err == nil && oldOpt.ID > 0 {
					if strings.TrimSpace(oldOpt.Keywords) != "" {
						var targetOpt models.CategoryOption
						if err := tx.Where("scope = ? AND name = ?", scope, newName).First(&targetOpt).Error; err == nil && targetOpt.ID > 0 {
							merged := mergeKeywordStrings(targetOpt.Keywords, oldOpt.Keywords)
							if merged != targetOpt.Keywords {
								if err := tx.Model(&targetOpt).Update("keywords", merged).Error; err != nil {
									return err
								}
							}
						}
					}
					if err := tx.Delete(&oldOpt).Error; err != nil {
						return err
					}
				}
			}
			return nil
		}

		if err := migrateScope("expense", expenseMigrations); err != nil {
			return err
		}
		if err := migrateScope("income", incomeMigrations); err != nil {
			return err
		}

		// 清理隐藏系统分类可能残留的 group_name
		if err := tx.Model(&models.CategoryOption{}).
			Where("scope = ? AND name IN ?", "expense", HiddenSystemExpenseNames()).
			Update("group_name", "").Error; err != nil {
			return err
		}

		return tx.Model(&models.SiteConfig{}).Where("id = ?", cfg.ID).
			Update("category_catalog_version", 3).Error
	})
}

// seedData 初始化站点全局配置与主理人初始档案 (空态冷启动 RG-05, RC-50, RC-52, RC-53)
func seedData(database *gorm.DB) {
	// 1. 站点全局配置 (全新部署置未初始化与纯净空态 RG-01, RC-50, RC-52)
	var cfgCount int64
	database.Model(&models.SiteConfig{}).Count(&cfgCount)
	if cfgCount == 0 {
		config := models.SiteConfig{
			SiteName:        "散页 · 个人生活志",
			SiteSubtitle:    "日常 · 记账 · 心愿",
			CuratorName:     "",
			DefaultLocation: "",
			SiteDesc:        "记录日常片段，打理收支账本与心愿清单。",
			Initialized:     false,
		}
		database.Create(&config)
	}

	// 2. 初始化主理人身份中枢 (Curator Profile 纯净空态 RC-52)
	var profileCount int64
	database.Model(&models.CuratorProfile{}).Count(&profileCount)
	if profileCount == 0 {
		profile := models.CuratorProfile{
			Name:        "",
			Title:       "",
			Bio:         "",
			Location:    "",
			CurrentCity: "",
			AvatarURL:   "",
			Motto:       "",
			Manifesto:   "",
			SocialLinks: `{}`,
		}
		database.Create(&profile)
	}

	// 3. 初始化默认内置信贷分期渠道 (花呗 10 号、京东白条 20 号 RC-53)
	var channelCount int64
	database.Model(&models.CreditChannel{}).Count(&channelCount)
	if channelCount == 0 {
		channels := []models.CreditChannel{
			{
				Code:         "huabei",
				Name:         "花呗",
				RepaymentDay: 10,
				IsBuiltin:    true,
				Color:        "var(--accent-warm)",
			},
			{
				Code:         "baitiao",
				Name:         "京东白条",
				RepaymentDay: 20,
				IsBuiltin:    true,
				Color:        "var(--accent-vermilion)",
			},
		}
		for _, ch := range channels {
			database.Create(&ch)
		}
	}

	// 4. 初始化三大模块的默认分类字典，并对老库执行一次幂等 v3 分类升级
	SeedDefaultCategories(database)
	UpgradeCategoryCatalogV3(database)

	log.Println("Database schema and core configurations ready.")
}
