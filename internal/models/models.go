package models

import (
	"time"

	"gorm.io/gorm"
)

// LifeEntry 统一生活时间线档案条目 (Life Stream)
// 支持 5 种核心生活形态：life(生活记录), thought(想法), photo(照片), project(项目), collection(收藏)
type LifeEntry struct {
	ID          uint         `gorm:"primaryKey;index:idx_entry_occurred_id,priority:2" json:"id"`
	Type        string       `gorm:"index" json:"type"` // life, thought, photo, project, collection (兼容原有子类型)
	Title       string       `json:"title"`
	Content     string       `gorm:"type:text" json:"content"`
	OccurredAt  time.Time    `gorm:"index:idx_entry_occurred_id,priority:1;index" json:"occurred_at"`
	Location    string       `json:"location"`
	ProjectID   *uint        `gorm:"index" json:"project_id"`
	Project     *Project     `gorm:"foreignKey:ProjectID" json:"project,omitempty"`
	Attachments []Attachment `gorm:"foreignKey:EntryID;constraint:OnDelete:CASCADE" json:"attachments"`

	Meta     string `json:"meta,omitempty"`
	Tags     string `json:"tags,omitempty"`
	Link     string `json:"link,omitempty"`
	Featured bool   `json:"featured,omitempty"`
	CityCode string `gorm:"size:12;index" json:"city_code,omitempty"`
	CityName string `gorm:"size:64" json:"city_name,omitempty"`

	CreatedAt time.Time      `json:"created_at"`
	UpdatedAt time.Time      `gorm:"index" json:"updated_at"`
	DeletedAt gorm.DeletedAt `gorm:"index" json:"deleted_at,omitempty"`
}

// Attachment 附件与多媒体对象关联表
type Attachment struct {
	ID               uint      `gorm:"primaryKey" json:"id"`
	EntryID          *uint     `gorm:"index" json:"entry_id,omitempty"`
	WishID           *uint     `gorm:"index" json:"wish_id,omitempty"`
	FootprintID      *uint     `gorm:"index" json:"footprint_id,omitempty"`
	ObjectKey        string    `gorm:"uniqueIndex" json:"object_key"`
	OriginalFilename string    `json:"original_filename"`
	MimeType         string    `json:"mime_type"`
	Size             int64     `json:"size"`
	Width            int       `json:"width,omitempty"`
	Height           int       `json:"height,omitempty"`
	URL              string    `gorm:"-" json:"url"` // 动态计算得出的公开或代理可访问 URL
	CreatedAt        time.Time `json:"created_at"`
}

// Project 长期进行的事情、创造与造物
type Project struct {
	ID          uint           `gorm:"primaryKey" json:"id"`
	Title       string         `json:"title"`
	Subtitle    string         `json:"subtitle,omitempty"`
	Description string         `json:"description"`
	Story       string         `gorm:"type:text" json:"story,omitempty"`
	Category    string         `json:"category,omitempty"`
	Tags        string         `json:"tags,omitempty"`
	GithubURL   string         `json:"github_url,omitempty"`
	Status      string         `json:"status,omitempty"`
	Featured    bool           `json:"featured,omitempty"`
	Order       int            `json:"order,omitempty"`
	CreatedAt   time.Time      `json:"created_at"`
	UpdatedAt   time.Time      `json:"updated_at"`
	DeletedAt   gorm.DeletedAt `gorm:"index" json:"deleted_at,omitempty"`
}

// Transaction 独立记账记录 (单位：分，避免浮点数精度误差，如 1250 表示 12.50 元)
type Transaction struct {
	ID                      uint           `gorm:"primaryKey;index:idx_tx_occurred_id,priority:2" json:"id"`
	Amount                  int64          `gorm:"not null" json:"amount"` // 分
	Type                    string         `gorm:"index;not null" json:"type"` // "income" 或 "expense"
	Category                string         `gorm:"index;not null" json:"category"` // 餐饮, 交通, 购物, 娱乐, 学习, 住房, 医疗, 其他
	Title                   string         `json:"title"`
	Note                    string         `json:"note"`
	PaymentMethod           string         `json:"payment_method"`
	WishID                  *uint          `gorm:"index" json:"wish_id,omitempty"`
	CityCode                string         `gorm:"size:12;index" json:"city_code,omitempty"`
	OccurredAt              time.Time      `gorm:"index:idx_tx_occurred_id,priority:1;index;not null" json:"occurred_at"`
	// 分期付款核心字段 (RC-53)
	IsInstallment           bool           `gorm:"default:false;index" json:"is_installment"`
	InstallmentChannelCode  string         `gorm:"size:32;index" json:"installment_channel_code,omitempty"` // huabei, baitiao, etc.
	InstallmentTotalAmount  int64          `gorm:"default:0" json:"installment_total_amount"` // 分期总额（分）
	InstallmentMonths       int            `gorm:"default:0" json:"installment_months"`       // 总期数 (3/6/12/24)
	InstallmentMonthlyPay   int64          `gorm:"default:0" json:"installment_monthly_pay"`  // 每期还款额（分）
	InstallmentRemainMonths int            `gorm:"default:0" json:"installment_remain_months"` // 剩余期数
	RepaymentDay            int            `gorm:"default:10" json:"repayment_day"`           // 绑定渠道的每月独立还款日 (1~31)
	CreatedAt               time.Time      `json:"created_at"`
	UpdatedAt               time.Time      `json:"updated_at"`
	DeletedAt               gorm.DeletedAt `gorm:"index" json:"deleted_at,omitempty"`
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

// GearItem 随身设备与固定资产模型 (RC-53, RC-79, RC-87)
type GearItem struct {
	ID             string `json:"id"`
	Name           string `json:"name"`
	Spec           string `json:"spec,omitempty"`
	Category       string `json:"category"`
	Status         string `json:"status,omitempty"` // active | sold (RC-90)
	PriceCents     int64  `json:"price_cents,omitempty"`
	PurchaseDate   string `json:"purchase_date,omitempty"`
	SoldPriceCents int64  `json:"sold_price_cents,omitempty"`
	SoldDate       string `json:"sold_date,omitempty"`
	Note           string `json:"note,omitempty"`
	Link           string `json:"link,omitempty"`
}

// SiteConfig 站点全局配置 (RC-80)
type SiteConfig struct {
	ID                   uint      `gorm:"primaryKey" json:"id"`
	SiteName             string    `json:"site_name"`
	SiteSubtitle         string    `json:"site_subtitle,omitempty"`
	CuratorName          string    `json:"curator_name,omitempty"`
	DefaultLocation      string    `json:"default_location,omitempty"`
	SiteDesc             string    `json:"site_desc,omitempty"`
	Initialized          bool      `gorm:"default:false" json:"initialized"`
	MasterPasscode       string    `gorm:"size:128" json:"-"`                       // 主理人记忆口令哈希
	InitialLiquidBalance int64     `gorm:"default:0" json:"initial_liquid_balance"` // 初始流动资金底数 (分 BE-15)
	CreatedAt            time.Time `json:"created_at"`
	UpdatedAt            time.Time `json:"updated_at"`
}

// CuratorProfile 主理人档案与身份中枢 (全端动态获取，禁止前端硬编码)
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
	Email       string    `gorm:"-" json:"email"`  // 从 SocialLinks 提取平铺下发 (RG-09)
	Github      string    `gorm:"-" json:"github"` // 从 SocialLinks 提取平铺下发 (RG-09)
	SocialLinks string    `gorm:"type:text" json:"-"` // JSON 格式存储
	SocialData  any       `gorm:"-" json:"social_links"`
	EDCGear     string    `gorm:"type:text" json:"-"` // JSON 格式存储
	EDCGearData any       `gorm:"-" json:"edc_gear"`
	UpdatedAt   time.Time `json:"updated_at"`
	CreatedAt   time.Time `json:"created_at"`
}

// Device 受信任设备表 (用于 macOS 桌面端、移动端免密配对组)
type Device struct {
	ID           uint      `gorm:"primaryKey" json:"id"`
	DeviceID     string    `gorm:"uniqueIndex;size:64;not null" json:"device_id"` // e.g. "dev_ios_48b19a27"
	Name         string    `json:"name"`                                         // e.g. "iPhone 16 Pro"
	Platform     string    `gorm:"index;size:32" json:"platform"`                // ios, macos, android, web
	DeviceModel  string    `json:"device_model,omitempty"`                       // e.g. "iPhone17,1"
	TokenHash    string    `gorm:"uniqueIndex;size:64;not null" json:"-"`        // SHA-256 哈希，绝不存明文
	Status       string    `gorm:"index;default:active;size:32" json:"status"`   // active, revoked
	PairedAt     time.Time `json:"paired_at"`
	LastActiveAt time.Time `gorm:"index" json:"last_active_at"`
	CreatedAt    time.Time `json:"created_at"`
	UpdatedAt    time.Time `json:"updated_at"`
}

// PairTicket 临时设备配对工单 (5分钟有效，单次核销 BE-19)
type PairTicket struct {
	ID             uint       `gorm:"primaryKey" json:"id"`
	Ticket         string     `gorm:"uniqueIndex;size:64;not null" json:"ticket"` // e.g. "pair_tk_9f82d1c6e4b8"
	PinCode        string     `gorm:"index;size:16;not null" json:"pin_code"`     // e.g. "749210"
	HubURL         string     `gorm:"size:255" json:"hub_url,omitempty"`
	PairedDeviceID string     `gorm:"size:64" json:"paired_device_id,omitempty"` // 核销该工单的设备标识
	WebToken       string     `gorm:"size:128" json:"-"`                         // 首次装帧伴生 Web 设备 Token (单次读取 BE-19)
	ExpiresAt      time.Time  `gorm:"index;not null" json:"expires_at"`
	Used           bool       `gorm:"index;default:false" json:"used"`
	UsedAt         *time.Time `json:"used_at,omitempty"`
	CreatedAt      time.Time  `json:"created_at"`
}

// Wish 三维心愿单表 (好物 item | 旅程 travel | 阶段里程碑 milestone)
type Wish struct {
	ID             uint           `gorm:"primaryKey" json:"id"`
	Title          string         `gorm:"size:255;not null" json:"title"`
	Kind           string         `gorm:"size:32;index;not null" json:"kind"` // item | travel | milestone
	Note           string         `gorm:"type:text" json:"note"`
	Spec           string         `gorm:"size:255" json:"spec,omitempty"`
	CityCode       string         `gorm:"size:12;index" json:"city_code,omitempty"`
	CityName       string         `gorm:"size:64" json:"city_name,omitempty"`
	Budget         int64          `gorm:"default:0" json:"budget"`      // 单位：分
	ActualCost     int64          `gorm:"default:0" json:"actual_cost"` // 单位：分
	ResolutionType string         `gorm:"size:32;default:exact" json:"resolution_type"` // exact | altered | relinquished
	ActualTitle    string         `gorm:"size:255" json:"actual_title,omitempty"`
	TargetDate     *time.Time     `json:"target_date,omitempty"`
	Status         string         `gorm:"size:32;index;default:wanting" json:"status"` // wanting | fulfilled | relinquished | shelved
	FulfilledAt    *time.Time     `json:"fulfilled_at,omitempty"`
	TransactionID  *uint          `gorm:"index" json:"transaction_id,omitempty"`
	LifeEntryID    *uint          `gorm:"index" json:"life_entry_id,omitempty"`
	FootprintID    *uint          `gorm:"index" json:"footprint_id,omitempty"`
	GearID         *string        `gorm:"size:64" json:"gear_id,omitempty"`
	Attachments    []Attachment   `gorm:"foreignKey:WishID" json:"attachments"`
	CreatedAt      time.Time      `json:"created_at"`
	UpdatedAt      time.Time      `json:"updated_at"`
	DeletedAt      gorm.DeletedAt `gorm:"index" json:"deleted_at,omitempty"`
}

// Footprint 城市旅游足迹打卡表 (RC-78: 移除无调用方且泄露精确定位的经纬度)
type Footprint struct {
	ID          uint           `gorm:"primaryKey" json:"id"`
	CityCode    string         `gorm:"size:12;index;not null" json:"city_code"` // 6 位行政区划代码，如杭州 330100
	CityName    string         `gorm:"size:64;not null" json:"city_name"`       // 城市名称，如 "杭州市"
	VisitedAt   time.Time      `gorm:"index;not null" json:"visited_at"`        // 到访日期
	Note        string         `gorm:"type:text" json:"note"`                   // 足迹随笔或简注
	LifeEntryID *uint          `gorm:"index" json:"life_entry_id,omitempty"`    // 可选关联时间流条目
	WishID      *uint          `gorm:"index" json:"wish_id,omitempty"`          // 可选关联溯源心愿
	Attachments []Attachment   `gorm:"foreignKey:FootprintID" json:"attachments"`
	CreatedAt   time.Time      `json:"created_at"`
	UpdatedAt   time.Time      `json:"updated_at"`
	DeletedAt   gorm.DeletedAt `gorm:"index" json:"deleted_at,omitempty"`
}


