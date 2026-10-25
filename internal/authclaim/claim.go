// Package authclaim 管理仅在进程内存中存在的一次性设备认领码。
//
// 设计约束：
//   - 认领码绝不落库、绝不写日志文件，只在启动或轮换时打印到标准输出；
//   - 进程重启即重新生成，旧码永久失效；
//   - 有效期 15 分钟，过期后只能重启服务获取新码；
//   - 连续输错 5 次立即作废并轮换新码；
//   - 比对使用常数时间算法，状态由互斥锁保护。
package authclaim

import (
	"crypto/rand"
	"crypto/subtle"
	"errors"
	"io"
	"strings"
	"sync"
	"time"
)

const (
	// CodeLength 认领码字符数（不含展示用连字符）
	CodeLength = 8
	// TTL 认领码有效期
	TTL = 15 * time.Minute
	// MaxFailures 连续输错多少次后立即作废并轮换
	MaxFailures = 5
	// alphabet Crockford Base32 去掉易混字符 0/O/1/I/L/U 后的 30 字符字母表
	alphabet = "23456789ABCDEFGHJKMNPQRSTVWXYZ"
)

var (
	// ErrNoActiveCode 当前没有可用的认领码（已过期、已消费或从未生成）
	ErrNoActiveCode = errors.New("authclaim: no active claim code")
	// ErrExpired 认领码已过期
	ErrExpired = errors.New("authclaim: claim code expired")
	// ErrInvalid 认领码不正确
	ErrInvalid = errors.New("authclaim: claim code is invalid")
	// ErrTooManyFailures 连续输错次数达到上限，旧码已作废并生成新码
	ErrTooManyFailures = errors.New("authclaim: too many failed attempts")
)

// Manager 保存当前认领码及其失败计数。
//
// 所有公开方法都通过 mu 串行化，因此可以安全地被多个 HTTP 请求并发调用。
type Manager struct {
	mu        sync.Mutex
	code      string
	expiresAt time.Time
	failures  int
	now       func() time.Time
	random    io.Reader
}

// Option 自定义 Manager 的可测试依赖。
type Option func(*Manager)

// WithClock 注入时间源（测试用）。
func WithClock(now func() time.Time) Option {
	return func(m *Manager) {
		if now != nil {
			m.now = now
		}
	}
}

// WithRand 注入随机源（测试用）。
func WithRand(r io.Reader) Option {
	return func(m *Manager) {
		if r != nil {
			m.random = r
		}
	}
}

// NewManager 创建认领码管理器。
func NewManager(opts ...Option) *Manager {
	m := &Manager{
		now:    time.Now,
		random: rand.Reader,
	}
	for _, opt := range opts {
		opt(m)
	}
	return m
}

// Generate 生成一枚全新认领码并重置失败计数，返回原始码与过期时间。
func (m *Manager) Generate() (string, time.Time, error) {
	code, err := generateCode(m.random)
	if err != nil {
		return "", time.Time{}, err
	}

	m.mu.Lock()
	defer m.mu.Unlock()
	m.code = code
	m.expiresAt = m.now().Add(TTL)
	m.failures = 0
	return code, m.expiresAt, nil
}

// Verify 校验用户输入。
//
// 返回值 rotated 仅在 ErrTooManyFailures 时非空，表示轮换后的新码，
// 调用方应立即打印给用户。成功时认领码被消费，返回 (\"\", nil)。
func (m *Manager) Verify(input string) (rotated string, err error) {
	m.mu.Lock()
	defer m.mu.Unlock()

	if m.code == "" {
		return "", ErrNoActiveCode
	}

	now := m.now()
	if !now.Before(m.expiresAt) {
		// 过期后只能重启服务重新生成，这里直接清空，避免继续接受输入。
		m.code = ""
		m.failures = 0
		return "", ErrExpired
	}

	normalized := Normalize(input)
	if subtle.ConstantTimeCompare([]byte(normalized), []byte(m.code)) == 1 {
		m.code = ""
		m.failures = 0
		return "", nil
	}

	m.failures++
	if m.failures >= MaxFailures {
		newCode, genErr := generateCode(m.random)
		if genErr != nil {
			m.code = ""
			m.failures = 0
			return "", genErr
		}
		m.code = newCode
		m.expiresAt = now.Add(TTL)
		m.failures = 0
		return newCode, ErrTooManyFailures
	}

	return "", ErrInvalid
}

// Normalize 将用户输入归一化为大写并去掉连字符与空白。
func Normalize(input string) string {
	var b strings.Builder
	b.Grow(len(input))
	for _, r := range strings.ToUpper(strings.TrimSpace(input)) {
		switch r {
		case '-', ' ', '\t', '\n', '\r', '_':
			continue
		default:
			b.WriteRune(r)
		}
	}
	return b.String()
}

// Format 将 8 位认领码展示为 XXXX-XXXX；长度不符时原样返回归一化结果。
func Format(code string) string {
	normalized := Normalize(code)
	if len(normalized) != CodeLength {
		return normalized
	}
	return normalized[:4] + "-" + normalized[4:]
}

// generateCode 从 30 字符字母表中均匀抽取 8 个字符。
//
// 采用拒绝采样避免 256 % 30 的取模偏差；random 读取失败时返回错误。
func generateCode(random io.Reader) (string, error) {
	// 30 * 8 = 240，240..255 拒绝，保证每个字符概率严格相等。
	const limit = byte(240)
	out := make([]byte, 0, CodeLength)
	buf := make([]byte, CodeLength*2)

	for len(out) < CodeLength {
		if _, err := io.ReadFull(random, buf); err != nil {
			return "", err
		}
		for _, b := range buf {
			if b >= limit {
				continue
			}
			out = append(out, alphabet[int(b)%len(alphabet)])
			if len(out) == CodeLength {
				break
			}
		}
	}
	return string(out), nil
}
