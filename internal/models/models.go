package models

import (
	"time"

	"gorm.io/gorm"
)

// LifeEntry 统一生活时间线档案条目 (Life Stream)
// 支持生活记录形态：life(日常), wish(心愿)
type LifeEntry struct {
	ID         uint      `gorm:"primaryKey;index:idx_entry_occurred_id,priority:2" json:"id"`
	Type       string    `gorm:"index;index:idx_entry_type_occurred,priority:1" json:"type"` // life, wish
	Category   string    `gorm:"size:32;index" json:"category"`                              // "", Photography, Travel, Still Life, Motion
	Title      string    `json:"title"`
	Content    string    `gorm:"type:text" json:"content"`
	OccurredAt time.Time `gorm:"index:idx_entry_occurred_id,priority:1;index:idx_entry_type_occurred,priority:2;index" json:"occurred_at"`
	Location   string    `json:"location"`
	WishID     *uint     `gorm:"index" json:"wish_id,omitempty"`
	Wish       *Wish     `gorm:"-" json:"wish,omitempty"` // 纯逻辑外键关联 WishID，不建物理外键约束
	// ClientRequestID 幂等创建标识（跨端统一契约 B）：
	// 客户端为每次创建生成稳定 UUID；同一 id 的重复提交直接返回已有记录，不重复落库。
	// 指针 + NULL 语义让历史行不参与唯一约束（SQLite 唯一索引中 NULL 互不冲突）。
	ClientRequestID *string      `gorm:"size:64;uniqueIndex" json:"client_request_id,omitempty"`
	Attachments     []Attachment `gorm:"-" json:"attachments"` // 纯逻辑外键关联 EntryID，不建物理外键与级联删除约束

	IsPrivate bool `gorm:"default:false;index" json:"is_private"`

	CreatedAt time.Time      `json:"created_at"`
	UpdatedAt time.Time      `gorm:"index" json:"updated_at"`
	DeletedAt gorm.DeletedAt `gorm:"index" json:"deleted_at,omitempty"`
}

// Attachment 附件与多媒体对象关联表
type Attachment struct {
	ID               uint   `gorm:"primaryKey" json:"id"`
	EntryID          *uint  `gorm:"index;index:idx_att_refs,priority:1" json:"entry_id,omitempty"`
	WishID           *uint  `gorm:"index;index:idx_att_refs,priority:2" json:"wish_id,omitempty"`
	ObjectKey        string `gorm:"uniqueIndex" json:"object_key"`
	OriginalFilename string `json:"original_filename"`
	MimeType         string `gorm:"index" json:"mime_type"`
	Size             int64  `json:"size"`
	Width            int    `json:"width,omitempty"`
	Height           int    `json:"height,omitempty"`
	URL              string `gorm:"-" json:"url"` // 动态计算得出的公开或代理可访问 URL
	// ThumbnailURL 缩略图地址（中枢自己的路由，稳定可缓存）。
	// 关键：列表与详情用缩略图，只有灯箱才用原图 ——
	// 原图动辄 1.5MB，从对象存储拉一次要 4 秒，时间流滚动时会把网络打满。
	ThumbnailURL string    `gorm:"-" json:"thumbnail_url,omitempty"`
	CreatedAt    time.Time `json:"created_at"`
}

// Transaction 独立记账记录 (单位：分，避免浮点数精度误差，如 1250 表示 12.50 元)
type Transaction struct {
	ID            uint      `gorm:"primaryKey;index:idx_tx_occurred_id,priority:2" json:"id"`
	Amount        int64     `gorm:"not null" json:"amount"`                                           // 分
	Type          string    `gorm:"index;index:idx_tx_type_occurred,priority:1;not null" json:"type"` // "income" 或 "expense"
	Category      string    `gorm:"index;not null" json:"category"`                                   // 餐饮, 交通, 购物, 娱乐, 学习, 住房, 医疗, 其他
	Title         string    `json:"title"`
	Note          string    `json:"note"`
	PaymentMethod string    `json:"payment_method"`
	WishID        *uint     `gorm:"index" json:"wish_id,omitempty"`
	OccurredAt    time.Time `gorm:"index:idx_tx_occurred_id,priority:1;index:idx_tx_type_occurred,priority:2;index;not null" json:"occurred_at"`
	// 分期付款核心字段 (RC-53)
	IsInstallment           bool   `gorm:"default:false;index" json:"is_installment"`
	InstallmentChannelCode  string `gorm:"size:32;index" json:"installment_channel_code,omitempty"` // huabei, baitiao, etc.
	InstallmentTotalAmount  int64  `gorm:"default:0" json:"installment_total_amount"`               // 分期总额（分）
	InstallmentMonths       int    `gorm:"default:0" json:"installment_months"`                     // 总期数 (3/6/12/12)
	InstallmentMonthlyPay   int64  `gorm:"default:0" json:"installment_monthly_pay"`                // 每期还款额（分）
	InstallmentRemainMonths int    `gorm:"default:0" json:"installment_remain_months"`              // 剩余期数
	RepaymentDay            int    `gorm:"default:10" json:"repayment_day"`                         // 绑定渠道的每月独立还款日 (1~31)
	// 真实分期流水扩展 (RC-140)
	Source            string `gorm:"size:32;index" json:"source,omitempty"`      // "installment_repay" | "installment_purchase"
	InstallmentPlanID *uint  `gorm:"index" json:"installment_plan_id,omitempty"` // 关联分期计划 ID
	InstallmentItemID *uint  `gorm:"index" json:"installment_item_id,omitempty"` // 关联分期期次 ID
	PrincipalAmount   int64  `gorm:"default:0" json:"principal_amount"`          // 还款本金（分）
	FeeAmount         int64  `gorm:"default:0" json:"fee_amount"`                // 还款手续费（分）
	// ClientRequestID 幂等创建标识（跨端统一契约 B，语义同 LifeEntry）
	ClientRequestID *string        `gorm:"size:64;uniqueIndex" json:"client_request_id,omitempty"`
	CreatedAt       time.Time      `json:"created_at"`
	UpdatedAt       time.Time      `gorm:"index" json:"updated_at"`
	DeletedAt       gorm.DeletedAt `gorm:"index" json:"deleted_at,omitempty"`
}

// CreditChannel 信贷与分期方式配置表 (RC-53)
type CreditChannel struct {
	ID           uint      `gorm:"primaryKey" json:"id"`
	Code         string    `gorm:"uniqueIndex;size:32;not null" json:"code"` // huabei, baitiao, custom_xxx
	Name         string    `gorm:"size:64;not null" json:"name"`             // "花呗", "京东白条", "招行信用卡"
	RepaymentDay int       `gorm:"not null" json:"repayment_day"`            // 每月独立还款日 (1~31)
	IsBuiltin    bool      `gorm:"default:false" json:"is_builtin"`          // 是否系统内置 (花呗/白条为 true)
	Color        string    `gorm:"size:32" json:"color,omitempty"`           // 标签色彩胶囊 (纸墨/朱砂/靛蓝)
	CreatedAt    time.Time `json:"created_at"`
	UpdatedAt    time.Time `json:"updated_at"`
}

// InstallmentPlan 分期还款计划主表 (RC-140)
type InstallmentPlan struct {
	ID              uint           `gorm:"primaryKey" json:"id"`
	ChannelID       uint           `gorm:"index;not null" json:"channel_id"`
	ChannelCode     string         `gorm:"size:32;index;not null" json:"channel_code"`            // huabei, baitiao, custom_xxx
	ChannelName     string         `gorm:"size:64" json:"channel_name,omitempty"`                 // 冗余渠道名，方便直显
	Title           string         `gorm:"size:128;not null" json:"title"`                        // 计划名称，如 "MacBook Pro 16"
	Kind            string         `gorm:"size:16;not null;default:'expense'" json:"kind"`        // "expense" (一笔消费)
	Principal       int64          `gorm:"not null" json:"principal"`                             // 总本金（分）
	Periods         int            `gorm:"not null" json:"periods"`                               // 总期数 (3, 6, 12, 24 等)
	FeeMode         string         `gorm:"size:32;not null" json:"fee_mode"`                      // "none" | "fixed_per_period" | "rate_per_period" | "total_fee"
	FeeValue        int64          `gorm:"default:0" json:"fee_value"`                            // 手续费数值：分，或基点/万分比 (如 50 代表 0.50%)
	TotalFee        int64          `gorm:"default:0" json:"total_fee"`                            // 计划总手续费（分）
	FirstDueDate    string         `gorm:"size:10;not null" json:"first_due_date"`                // 首期还款日 YYYY-MM-DD
	RepayDay        int            `gorm:"not null" json:"repay_day"`                             // 每月还款日 (1~31)
	CategoryID      *uint          `gorm:"index" json:"category_id,omitempty"`                    // 关联分类 ID
	CategoryName    string         `gorm:"size:64" json:"category_name,omitempty"`                // 关联分类名称
	PurchaseEntryID *uint          `gorm:"index" json:"purchase_entry_id,omitempty"`              // 生成的购买账目 ID
	Status          string         `gorm:"size:16;index;not null;default:'active'" json:"status"` // "active" | "settled" | "cancelled"
	Note            string         `json:"note,omitempty"`
	CreatedAt       time.Time      `json:"created_at"`
	UpdatedAt       time.Time      `gorm:"index" json:"updated_at"`
	DeletedAt       gorm.DeletedAt `gorm:"index" json:"deleted_at,omitempty"`
}

// InstallmentItem 分期计划期次明细表 (RC-140)
type InstallmentItem struct {
	ID            uint       `gorm:"primaryKey" json:"id"`
	PlanID        uint       `gorm:"index:idx_plan_seq,priority:1;not null" json:"plan_id"`
	Seq           int        `gorm:"index:idx_plan_seq,priority:2;not null" json:"seq"`        // 第几期 (1..periods)
	DueDate       string     `gorm:"size:10;index;not null" json:"due_date"`                   // 到期日 YYYY-MM-DD
	PrincipalPart int64      `gorm:"not null" json:"principal_part"`                           // 本期应还本金（分）
	FeePart       int64      `gorm:"not null" json:"fee_part"`                                 // 本期应还手续费（分）
	Status        string     `gorm:"size:16;index;not null;default:'scheduled'" json:"status"` // "scheduled" | "paid"
	PaidAt        *time.Time `json:"paid_at,omitempty"`                                        // 实际还款时间
	PaidAmount    int64      `gorm:"default:0" json:"paid_amount"`                             // 实际支付金额（分）
	PayAccount    string     `gorm:"size:64" json:"pay_account,omitempty"`                     // 付款账户（如 "招商银行储蓄卡"）
	TransactionID *uint      `gorm:"index" json:"transaction_id,omitempty"`                    // 关联生成的还款账目 ID
	CreatedAt     time.Time  `json:"created_at"`
	UpdatedAt     time.Time  `json:"updated_at"`
}

// CategoryOption 用户自定义分类与字典配置表
// Scope: "expense"(支出分类), "income"(收入分类), "payment"(支付方式)
type CategoryOption struct {
	ID         uint      `gorm:"primaryKey" json:"id"`
	Scope      string    `gorm:"index:idx_cat_scope_name,unique;size:32;not null" json:"scope"`
	Name       string    `gorm:"index:idx_cat_scope_name,unique;size:64;not null" json:"name"`
	GroupName  string    `gorm:"size:32;default:''" json:"group_name,omitempty"`
	Keywords   string    `gorm:"type:text;default:''" json:"keywords,omitempty"`
	SortOrder  int       `gorm:"default:0;index" json:"sort_order"`
	IsBuiltin  bool      `gorm:"default:false" json:"is_builtin"`
	UsageCount int64     `gorm:"-" json:"usage_count"`
	CreatedAt  time.Time `json:"created_at"`
	UpdatedAt  time.Time `json:"updated_at"`
}

// CategoryRule 商户/收款方自动归类记忆规则表
type CategoryRule struct {
	ID          uint      `gorm:"primaryKey" json:"id"`
	TxType      string    `gorm:"size:16;not null;uniqueIndex:idx_cat_rule_type_key" json:"tx_type"` // "expense" | "income"
	MatchKey    string    `gorm:"size:128;not null;uniqueIndex:idx_cat_rule_type_key" json:"match_key"`
	DisplayName string    `gorm:"size:128;not null" json:"display_name"`
	Category    string    `gorm:"size:64;not null;index" json:"category"`
	HitCount    int       `gorm:"default:1" json:"hit_count"`
	Source      string    `gorm:"size:16;default:'learned'" json:"source"` // "learned" | "manual"
	CreatedAt   time.Time `json:"created_at"`
	UpdatedAt   time.Time `json:"updated_at"`
}

// SiteConfig 站点全局配置 (RC-80)
type SiteConfig struct {
	ID                     uint     `gorm:"primaryKey" json:"id"`
	SiteName               string   `json:"site_name"`
	SiteSubtitle           string   `json:"site_subtitle,omitempty"`
	CuratorName            string   `json:"curator_name,omitempty"`
	DefaultLocation        string   `json:"default_location,omitempty"`
	SiteDesc               string   `json:"site_desc,omitempty"`
	Initialized            bool     `gorm:"default:false" json:"initialized"`
	CategoryCatalogVersion int      `gorm:"default:0" json:"-"`
	PublicModulesRaw       string   `gorm:"column:public_modules;type:text;default:'[]'" json:"-"`
	PublicModules          []string `gorm:"-" json:"public_modules"`

	// 对象存储动态配置 (支持通过管理后台在线配置 Cloudflare R2 / S3 兼容存储)
	R2Disabled             bool   `gorm:"default:false" json:"-"`
	R2AccountID            string `gorm:"size:128" json:"-"`
	R2AccessKeyID          string `gorm:"size:128" json:"-"`
	R2SecretAccessKey      string `gorm:"size:128" json:"-"` // 不在通用接口直接下发，避免泄露
	R2BucketName           string `gorm:"size:128" json:"-"`
	R2Endpoint             string `gorm:"size:255" json:"-"`
	R2PublicDomain         string `gorm:"size:255" json:"-"`
	R2PresignExpireSeconds int    `gorm:"default:1800" json:"-"`

	CreatedAt time.Time `json:"created_at"`
	UpdatedAt time.Time `json:"updated_at"`
}

// CuratorProfile 主理人基础信息配置 (全端动态获取，禁止前端硬编码)
type CuratorProfile struct {
	ID          uint      `gorm:"primaryKey" json:"id"`
	Name        string    `json:"name"`
	Title       string    `json:"title"`
	Bio         string    `gorm:"type:text" json:"bio"`
	Location    string    `json:"location"`
	CurrentCity string    `json:"current_city"`
	AvatarURL   string    `json:"avatar_url"`
	AvatarKey   string    `gorm:"-" json:"avatar_key,omitempty"` // 原始存储对象 Key / 相对路径 (如 avatar/user.png)
	Motto       string    `json:"motto"`
	Manifesto   string    `gorm:"type:text" json:"manifesto"`
	Email       string    `gorm:"-" json:"email"`     // 从 SocialLinks 提取平铺下发 (RG-09)
	Github      string    `gorm:"-" json:"github"`    // 从 SocialLinks 提取平铺下发 (RG-09)
	SocialLinks string    `gorm:"type:text" json:"-"` // JSON 格式存储
	SocialData  any       `gorm:"-" json:"social_links"`
	UpdatedAt   time.Time `json:"updated_at"`
	CreatedAt   time.Time `json:"created_at"`
}

// Device 登录会话与客户端凭证表 (Web Cookie / macOS / Android Bearer 会话)
type Device struct {
	ID           uint      `gorm:"primaryKey" json:"id"`
	DeviceID     string    `gorm:"uniqueIndex;size:64;not null" json:"device_id"` // e.g. "dev_ios_48b19a27"
	Name         string    `json:"name"`                                          // e.g. "iPhone 16 Pro"
	Platform     string    `gorm:"index;size:32" json:"platform"`                 // ios, macos, android, web
	DeviceModel  string    `json:"device_model,omitempty"`                        // e.g. "iPhone17,1"
	AccountSub   string    `gorm:"index;size:128" json:"account_sub,omitempty"`   // OIDC Subject 标识
	AccountName  string    `gorm:"size:128" json:"account_name,omitempty"`        // OIDC 账号显示名
	AccountEmail string    `gorm:"size:255" json:"account_email,omitempty"`       // OIDC 账号邮箱
	TokenHash    string    `gorm:"uniqueIndex;size:64;not null" json:"-"`         // SHA-256 哈希，绝不存明文
	Status       string    `gorm:"index;default:active;size:32" json:"status"`    // active, revoked
	IsCurrent    bool      `gorm:"-" json:"is_current"`                           // 当前请求所属会话
	PairedAt     time.Time `json:"paired_at"`
	LastActiveAt time.Time `gorm:"index" json:"last_active_at"`
	CreatedAt    time.Time `json:"created_at"`
	UpdatedAt    time.Time `json:"updated_at"`
}

// PairTicket 原生客户端 OAuth2/OIDC 一次性短效授权兑换码表（90 秒有效，单次原子核销）
type PairTicket struct {
	ID             uint       `gorm:"primaryKey" json:"id"`
	Ticket         string     `gorm:"uniqueIndex;size:64;not null" json:"ticket"` // e.g. "auth_code_9f82d1c6e4b8"
	PinCode        string     `gorm:"size:16;default:''" json:"-"`                // 兼容已有 SQLite 数据库中可能残留的 NOT NULL 约束，不对外暴露
	HubURL         string     `gorm:"size:255" json:"hub_url,omitempty"`
	AccountSub     string     `gorm:"size:128" json:"account_sub,omitempty"`
	AccountName    string     `gorm:"size:128" json:"account_name,omitempty"`
	AccountEmail   string     `gorm:"size:255" json:"account_email,omitempty"`
	PairedDeviceID string     `gorm:"size:64" json:"paired_device_id,omitempty"` // 核销该授权码的会话标识
	ExpiresAt      time.Time  `gorm:"index;not null" json:"expires_at"`
	Used           bool       `gorm:"index;default:false" json:"used"`
	UsedAt         *time.Time `json:"used_at,omitempty"`
	CreatedAt      time.Time  `json:"created_at"`
}

// Wish 心愿单表 (好物 item | 旅程 travel | 阶段里程碑 milestone)
type Wish struct {
	ID             uint       `gorm:"primaryKey" json:"id"`
	Title          string     `gorm:"size:255;not null" json:"title"`
	Kind           string     `gorm:"size:32;index;index:idx_wish_status_kind_created,priority:2;not null" json:"kind"` // item | travel | milestone
	Note           string     `gorm:"type:text" json:"note"`
	Spec           string     `gorm:"size:255" json:"spec,omitempty"`
	Budget         int64      `gorm:"default:0" json:"budget"`                      // 单位：分
	ActualCost     int64      `gorm:"default:0" json:"actual_cost"`                 // 单位：分
	ResolutionType string     `gorm:"size:32;default:exact" json:"resolution_type"` // exact | altered | relinquished
	ActualTitle    string     `gorm:"size:255" json:"actual_title,omitempty"`
	TargetDate     *time.Time `json:"target_date,omitempty"`
	// PlantedAt 许下心愿的时刻（可回填为过去日期以支撑「补档」）；
	// 伴生的一级散页以它作为 occurred_at，从而在时间流里落在正确的历史位置。
	PlantedAt     *time.Time   `gorm:"index" json:"planted_at,omitempty"`
	Status        string       `gorm:"size:32;index;index:idx_wish_status_kind_created,priority:1;index:idx_wish_status_fulfilled,priority:1;default:wanting" json:"status"` // wanting | fulfilled | relinquished | shelved
	IsPrivate     bool         `gorm:"default:false;index" json:"is_private"`
	FulfilledAt   *time.Time   `gorm:"index;index:idx_wish_status_fulfilled,priority:2" json:"fulfilled_at,omitempty"`
	TransactionID *uint        `gorm:"index" json:"transaction_id,omitempty"`
	LifeEntryID   *uint        `gorm:"index" json:"life_entry_id,omitempty"`
	Attachments   []Attachment `gorm:"-" json:"attachments"` // 纯逻辑外键关联 WishID，不建物理外键约束
	// ClientRequestID 幂等创建标识（跨端统一契约 B，语义同 LifeEntry）：
	// 客户端为每次创建生成稳定 UUID；同一 id 的重复提交直接返回已有记录。
	ClientRequestID *string        `gorm:"size:64;uniqueIndex" json:"client_request_id,omitempty"`
	CreatedAt       time.Time      `gorm:"index;index:idx_wish_status_kind_created,priority:3" json:"created_at"`
	UpdatedAt       time.Time      `gorm:"index" json:"updated_at"`
	DeletedAt       gorm.DeletedAt `gorm:"index" json:"deleted_at,omitempty"`
}

// SyncTombstone 增量同步墓碑表。
//
// 物理删除（?purge=true）会把原行从数据库里彻底抹掉，增量同步的
// `updated_at/deleted_at >= since` 过滤就再也看不到它 —— 离线超过软删窗口的
// 客户端会永久保留一条服务端已经不存在的记录。
//
// 因此 purge 时在同一事务里写一条最小墓碑（只保留 id / 类型 / 删除时刻），
// 增量列表接口在带 since 的请求里把墓碑合成回对应模型的 JSON 行，
// 客户端沿用现有的 deleted_at 合并逻辑即可删除本地行。
type SyncTombstone struct {
	ID         uint      `gorm:"primaryKey" json:"id"`
	EntityType string    `gorm:"size:32;not null;uniqueIndex:idx_sync_tombstone_entity" json:"entity_type"`
	EntityID   uint      `gorm:"not null;uniqueIndex:idx_sync_tombstone_entity" json:"entity_id"`
	DeletedAt  time.Time `gorm:"index;not null" json:"deleted_at"`
}
