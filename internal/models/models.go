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

	Meta           string `json:"meta,omitempty"`
	Tags           string `json:"tags,omitempty"`
	Link           string `json:"link,omitempty"`
	Featured       bool   `json:"featured,omitempty"`

	CreatedAt time.Time      `json:"created_at"`
	UpdatedAt time.Time      `gorm:"index" json:"updated_at"`
	DeletedAt gorm.DeletedAt `gorm:"index" json:"deleted_at,omitempty"`
}

// Attachment 附件与多媒体对象关联表
type Attachment struct {
	ID               uint      `gorm:"primaryKey" json:"id"`
	EntryID          *uint     `gorm:"index" json:"entry_id"`
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
	ID          uint      `gorm:"primaryKey" json:"id"`
	Title       string    `json:"title"`
	Subtitle    string    `json:"subtitle,omitempty"`
	Description string    `json:"description"`
	Story       string    `gorm:"type:text" json:"story,omitempty"`
	Category    string    `json:"category,omitempty"`
	Tags        string    `json:"tags,omitempty"`
	GithubURL   string    `json:"github_url,omitempty"`
	Status      string    `json:"status,omitempty"`
	Featured    bool      `json:"featured,omitempty"`
	Order       int       `json:"order,omitempty"`
	CreatedAt   time.Time `json:"created_at"`
	UpdatedAt   time.Time `json:"updated_at"`
}

// Transaction 独立记账记录 (单位：分，避免浮点数精度误差，如 1250 表示 12.50 元)
type Transaction struct {
	ID            uint      `gorm:"primaryKey;index:idx_tx_occurred_id,priority:2" json:"id"`
	Amount        int64     `gorm:"not null" json:"amount"` // 分
	Type          string    `gorm:"index;not null" json:"type"` // "income" 或 "expense"
	Category      string    `gorm:"index;not null" json:"category"` // 餐饮, 交通, 购物, 娱乐, 学习, 住房, 医疗, 其他
	Title         string    `json:"title"`
	Note          string    `json:"note"`
	PaymentMethod string    `json:"payment_method"`
	OccurredAt    time.Time `gorm:"index:idx_tx_occurred_id,priority:1;index;not null" json:"occurred_at"`
	CreatedAt     time.Time `json:"created_at"`
	UpdatedAt     time.Time `json:"updated_at"`
}

// SiteConfig 站点全局与备案合规配置
type SiteConfig struct {
	ID              uint      `gorm:"primaryKey" json:"id"`
	SiteName        string    `json:"site_name"`
	SiteSubtitle    string    `json:"site_subtitle,omitempty"`
	CuratorName     string    `json:"curator_name,omitempty"`
	CuratorEn       string    `json:"curator_en,omitempty"`
	DefaultLocation string    `json:"default_location,omitempty"`
	SiteDesc        string    `json:"site_desc,omitempty"`
	Domain          string    `json:"domain"`
	ICPNumber       string    `json:"icp_number"`    // 工信部 ICP 备案号
	ICPLink         string    `json:"icp_link"`      // https://beian.miit.gov.cn
	PoliceNumber    string    `json:"police_number,omitempty"` // 公安联网备案号
	PoliceCode      string    `json:"police_code,omitempty"`   // 公安备案编号
	Initialized     bool      `gorm:"default:false" json:"initialized"`
	MasterPasscode  string    `gorm:"size:128" json:"-"` // 主理人记忆口令哈希
	CreatedAt       time.Time `json:"created_at"`
	UpdatedAt       time.Time `json:"updated_at"`
}

// CuratorProfile 主理人档案与身份中枢 (全端动态获取，禁止前端硬编码)
type CuratorProfile struct {
	ID          uint      `gorm:"primaryKey" json:"id"`
	Name        string    `json:"name"`
	EnglishName string    `json:"english_name"`
	Title       string    `json:"title"`
	Bio         string    `gorm:"type:text" json:"bio"`
	Location    string    `json:"location"`
	CurrentCity string    `json:"current_city"`
	AvatarURL   string    `json:"avatar_url"`
	AvatarKey   string    `gorm:"-" json:"avatar_key,omitempty"` // 原始存储对象 Key / 相对路径 (如 avatar/user.png)
	Motto       string    `json:"motto"`
	Manifesto   string    `gorm:"type:text" json:"manifesto"`
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

// PairTicket 临时设备配对工单 (5分钟有效，单次核销)
type PairTicket struct {
	ID        uint       `gorm:"primaryKey" json:"id"`
	Ticket    string     `gorm:"uniqueIndex;size:64;not null" json:"ticket"` // e.g. "pair_tk_9f82d1c6e4b8"
	PinCode   string     `gorm:"index;size:16;not null" json:"pin_code"`     // e.g. "749210"
	ExpiresAt time.Time  `gorm:"index;not null" json:"expires_at"`
	Used      bool       `gorm:"index;default:false" json:"used"`
	UsedAt    *time.Time `json:"used_at,omitempty"`
	CreatedAt time.Time  `json:"created_at"`
}

// QRSession 网页端反向扫码登录会话 (2分钟有效)
type QRSession struct {
	ID        uint      `gorm:"primaryKey" json:"id"`
	SessionID string    `gorm:"uniqueIndex;size:64;not null" json:"session_id"` // e.g. "qr_sess_8910283b"
	Status    string    `gorm:"index;size:32;default:pending" json:"status"`   // pending, authorized, expired
	Token     string    `gorm:"size:128" json:"-"`                             // 授权通过后下发的管理 Token
	ExpiresAt time.Time `gorm:"index;not null" json:"expires_at"`
	CreatedAt time.Time `json:"created_at"`
}

