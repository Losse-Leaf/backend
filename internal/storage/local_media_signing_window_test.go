package storage

import (
	"strconv"
	"testing"
	"time"
)

// 签名 URL 的过期时间必须按 300 秒窗口对齐：
// 同一窗口内签发的 URL 完全一致（否则 immutable 缓存永不命中）；
// 跨窗口必须变化；且 exp 始终不早于 now+ttl。
func TestMediaSignatureExpiryWindowAlignment(t *testing.T) {
	const ttl = 30 * time.Minute
	base := time.Unix(1_700_000_000, 123_456_789)
	windowStart := time.Unix(base.Unix()/mediaSignatureWindowSeconds*mediaSignatureWindowSeconds, 0)
	windowEnd := windowStart.Add(299 * time.Second)

	expAtStart := mediaSignatureExpiry(windowStart, ttl)
	expAtEnd := mediaSignatureExpiry(windowEnd, ttl)
	if expAtStart != expAtEnd {
		t.Fatalf("同一 300s 窗口内 exp 必须一致：窗口起点 %d，窗口终点 %d", expAtStart, expAtEnd)
	}
	if expAtStart%mediaSignatureWindowSeconds != 0 {
		t.Fatalf("exp 必须落在 300s 窗口边界上，实际 %d", expAtStart)
	}
	if want := windowEnd.Unix() + int64(ttl/time.Second); expAtStart < want {
		t.Fatalf("exp 必须 >= now+ttl：窗口终点要求 %d，实际 %d", want, expAtStart)
	}

	// 窗口内任意时刻都应得到同一个 exp
	for _, offset := range []int64{0, 1, 42, 298, 299} {
		now := windowStart.Add(time.Duration(offset) * time.Second)
		if got := mediaSignatureExpiry(now, ttl); got != expAtStart {
			t.Errorf("窗口内 offset=%ds 的 exp 应恒为 %d，实际 %d", offset, expAtStart, got)
		}
		if expAtStart < now.Unix()+int64(ttl/time.Second) {
			t.Errorf("offset=%ds 时 exp=%d 小于 now+ttl", offset, expAtStart)
		}
	}

	// 跨窗口必须变化
	nextExp := mediaSignatureExpiry(windowStart.Add(300*time.Second), ttl)
	if nextExp == expAtStart {
		t.Fatalf("跨窗口的 exp 必须变化，实际仍为 %d", expAtStart)
	}
	if nextExp%mediaSignatureWindowSeconds != 0 {
		t.Fatalf("跨窗口 exp 也必须对齐，实际 %d", nextExp)
	}
}

// 原图与缩略图共用同一套窗口规则；连续两次签名结果完全一致。
func TestSignedMediaURLsAreStableWithinWindow(t *testing.T) {
	key := "entries/2026/10/21/test.jpg"
	// 使用当前窗口，确保签出的 URL 在测试执行时尚未过期
	base := time.Now()
	windowStart := time.Unix(base.Unix()/mediaSignatureWindowSeconds*mediaSignatureWindowSeconds, 0)
	windowEnd := windowStart.Add(299 * time.Second)

	media1 := signLocalMediaURLAt(key, mediaSignatureTTL, windowStart)
	media2 := signLocalMediaURLAt(key, mediaSignatureTTL, windowEnd)
	if media1 != media2 {
		t.Fatalf("同一窗口内原图 URL 必须一致：\n  %s\n  %s", media1, media2)
	}

	thumb1 := signLocalThumbnailURLAt(key, 480, windowStart)
	thumb2 := signLocalThumbnailURLAt(key, 480, windowEnd)
	if thumb1 != thumb2 {
		t.Fatalf("同一窗口内缩略图 URL 必须一致：\n  %s\n  %s", thumb1, thumb2)
	}

	// 两者都必须仍可验证
	exp, err := strconv.ParseInt(extractParam(media1, "exp"), 10, 64)
	if err != nil {
		t.Fatalf("原图 exp 无法解析: %v", err)
	}
	if exp < windowEnd.Unix()+int64(mediaSignatureTTL/time.Second) {
		t.Errorf("原图 exp=%d 小于 now+ttl", exp)
	}
	if !VerifyLocalMediaSignature(key, extractParam(media1, "exp"), extractParam(media1, "sig")) {
		t.Error("窗口内签发的原图 URL 应可验证")
	}
	if !VerifyLocalThumbnailSignature(key, 480, extractParam(thumb1, "exp"), extractParam(thumb1, "sig")) {
		t.Error("窗口内签发的缩略图 URL 应可验证")
	}

	// 跨窗口必须得到不同的 URL
	nextMedia := signLocalMediaURLAt(key, mediaSignatureTTL, windowStart.Add(300*time.Second))
	if nextMedia == media1 {
		t.Fatal("跨窗口的原图 URL 必须变化")
	}
	nextThumb := signLocalThumbnailURLAt(key, 480, windowStart.Add(300*time.Second))
	if nextThumb == thumb1 {
		t.Fatal("跨窗口的缩略图 URL 必须变化")
	}
}

// 窗口对齐不得放宽校验：过期签名仍然必须被拒绝。
func TestExpiredMediaSignaturesStillRejected(t *testing.T) {
	key := "entries/2026/10/21/test.jpg"
	now := time.Now()

	expired := now.Add(-time.Second).Unix()
	if VerifyLocalMediaSignature(key, strconv.FormatInt(expired, 10), computeMediaSignature(mediaResource(key), expired)) {
		t.Error("已过期的原图签名必须被拒绝")
	}
	if VerifyLocalThumbnailSignature(key, 480, strconv.FormatInt(expired, 10), computeMediaSignature(thumbnailResource(key, 480), expired)) {
		t.Error("已过期的缩略图签名必须被拒绝")
	}

	valid := now.Add(time.Minute).Unix()
	if !VerifyLocalMediaSignature(key, strconv.FormatInt(valid, 10), computeMediaSignature(mediaResource(key), valid)) {
		t.Error("未过期的原图签名必须通过")
	}
	if !VerifyLocalThumbnailSignature(key, 480, strconv.FormatInt(valid, 10), computeMediaSignature(thumbnailResource(key, 480), valid)) {
		t.Error("未过期的缩略图签名必须通过")
	}
}
