package handlers

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"mime/multipart"
	"net/http"
	"net/textproto"
	"net/url"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"loose-leaf/internal/config"
	"loose-leaf/internal/db"
	"loose-leaf/internal/models"
	"loose-leaf/internal/storage"
)

var spaceSyncHTTPClient = &http.Client{
	Timeout: 60 * time.Second,
}

func getVesperSpaceSyncConfig() (baseURL, bridgeToken string, enabled bool) {
	baseURL = strings.TrimRight(strings.TrimSpace(os.Getenv("VESPER_SPACE_URL")), "/")
	bridgeToken = strings.TrimSpace(os.Getenv("INTERNAL_BRIDGE_TOKEN"))
	enabled = baseURL != "" && bridgeToken != ""
	return
}

// VesperSpaceProjectionEnabled 返回是否启用了《散页》带图条目向 Vesper Space 的数据层自动投影。
func VesperSpaceProjectionEnabled() bool {
	_, _, ok := getVesperSpaceSyncConfig()
	return ok
}

func spaceWorkIDForAttachment(attID uint) string {
	return fmt.Sprintf("vs-leaf-att-%d", attID)
}

func isProjectableAttachment(att models.Attachment) bool {
	mime := strings.ToLower(strings.TrimSpace(att.MimeType))
	if strings.HasPrefix(mime, "image/") || strings.HasPrefix(mime, "video/") {
		return true
	}
	ext := strings.ToLower(filepath.Ext(att.ObjectKey))
	switch ext {
	case ".jpg", ".jpeg", ".png", ".webp", ".gif", ".avif", ".mp4", ".webm":
		return true
	default:
		return false
	}
}

func inferSpaceCategoryForEntry(entry models.LifeEntry, att models.Attachment) string {
	mime := strings.ToLower(strings.TrimSpace(att.MimeType))
	ext := strings.ToLower(filepath.Ext(att.ObjectKey))
	if strings.HasPrefix(mime, "video/") || ext == ".mp4" || ext == ".webm" {
		return "Motion"
	}
	if entry.Type == "wish" || (entry.WishID != nil && *entry.WishID > 0) {
		return "Still Life"
	}
	if strings.TrimSpace(entry.Location) != "" {
		return "Travel"
	}
	return "Photography"
}

func computeAttachmentAspect(att models.Attachment) float64 {
	if att.Width > 0 && att.Height > 0 {
		ratio := float64(att.Width) / float64(att.Height)
		if ratio < 0.45 {
			return 0.45
		}
		if ratio > 2.4 {
			return 2.4
		}
		return float64(int(ratio*100+0.5)) / 100
	}
	return 1.0
}

func resolveCuratorArtistName() string {
	if db.DB != nil {
		var siteCfg models.SiteConfig
		if err := db.DB.Limit(1).Find(&siteCfg).Error; err == nil {
			if name := strings.TrimSpace(siteCfg.CuratorName); name != "" {
				return name
			}
		}
	}
	if envName := strings.TrimSpace(os.Getenv("VESPER_SPACE_ARTIST")); envName != "" {
		return envName
	}
	return "迟暮"
}

func diffRemovedAttachmentIDs(oldIDs []uint, current []models.Attachment) []uint {
	if len(oldIDs) == 0 {
		return nil
	}
	keep := make(map[uint]bool, len(current))
	for _, att := range current {
		keep[att.ID] = true
	}
	var removed []uint
	for _, id := range oldIDs {
		if id > 0 && !keep[id] {
			removed = append(removed, id)
		}
	}
	return removed
}

func deleteSpaceWorkByAttachmentID(ctx context.Context, baseURL, bridgeToken string, attID uint, keepMedia bool) error {
	if attID == 0 {
		return nil
	}
	workID := spaceWorkIDForAttachment(attID)
	apiURL := fmt.Sprintf("%s/api/v1/works/%s", baseURL, url.PathEscape(workID))
	if keepMedia {
		apiURL += "?keepMedia=true"
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodDelete, apiURL, nil)
	if err != nil {
		return err
	}
	req.Header.Set("Authorization", "Bearer "+bridgeToken)
	req.Header.Set("X-Bridge-Token", bridgeToken)
	resp, err := spaceSyncHTTPClient.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	_, _ = io.Copy(io.Discard, resp.Body)
	if resp.StatusCode == http.StatusNotFound || (resp.StatusCode >= 200 && resp.StatusCode < 300) {
		return nil
	}
	return fmt.Errorf("delete space work %s returned HTTP %d", workID, resp.StatusCode)
}

func projectSingleAttachmentToSpace(ctx context.Context, baseURL, bridgeToken, artist string, entry models.LifeEntry, att models.Attachment, idx, total int) error {
	workID := spaceWorkIDForAttachment(att.ID)
	title := strings.TrimSpace(entry.Title)
	if title == "" {
		title = DeriveTitleFromContent(entry.Content, 40)
	}
	if title == "" {
		title = "散页随拍"
	}
	if total > 1 {
		title = fmt.Sprintf("%s (%d/%d)", title, idx+1, total)
	}

	occ := entry.OccurredAt
	if occ.IsZero() {
		occ = entry.CreatedAt
	}
	if config.BeijingLocation != nil {
		occ = occ.In(config.BeijingLocation)
	}
	year := strconv.Itoa(occ.Year())
	category := inferSpaceCategoryForEntry(entry, att)
	aspect := computeAttachmentAspect(att)
	visibility := "public"
	if entry.IsPrivate {
		visibility = "private"
	}
	desc := strings.TrimSpace(entry.Content)
	loc := strings.TrimSpace(entry.Location)

	// Path A: Zero-copy shared 5TB storage when same-host AList mirror is enabled
	if storage.AlistMirrorEnabled() {
		if alistPath, err := storage.MirrorLocalToAlist(ctx, att.ObjectKey, att.MimeType); err == nil && alistPath != "" {
			relUnderSpace := strings.TrimPrefix(alistPath, "/space/")
			relUnderSpace = strings.TrimPrefix(relUnderSpace, "/")
			mediaURL := fmt.Sprintf("%s/api/v1/media/%s", baseURL, storage.EscapeObjectKeyPath(relUnderSpace))
			payload := map[string]any{
				"id":               workID,
				"title":            title,
				"artist":           artist,
				"year":             year,
				"category":         category,
				"aspect":           aspect,
				"src":              mediaURL,
				"alist_path":       alistPath,
				"persist_to_drive": false,
				"visibility":       visibility,
				"description":      desc,
				"curator_note":     desc,
				"alt":              title,
			}
			if loc != "" {
				payload["location"] = loc
			}
			bodyBytes, _ := json.Marshal(payload)
			req, err := http.NewRequestWithContext(ctx, http.MethodPost, baseURL+"/api/v1/works", bytes.NewReader(bodyBytes))
			if err != nil {
				return err
			}
			req.Header.Set("Content-Type", "application/json")
			req.Header.Set("Authorization", "Bearer "+bridgeToken)
			req.Header.Set("X-Bridge-Token", bridgeToken)
			resp, err := spaceSyncHTTPClient.Do(req)
			if err != nil {
				return err
			}
			defer resp.Body.Close()
			raw, _ := io.ReadAll(io.LimitReader(resp.Body, 32*1024))
			if resp.StatusCode >= 200 && resp.StatusCode < 300 {
				return nil
			}
			return fmt.Errorf("space JSON upsert HTTP %d: %s", resp.StatusCode, strings.TrimSpace(string(raw)))
		}
	}

	// Path B: Fallback multipart upload of raw bytes to Vesper Space Worker
	rawBytes, err := storage.ReadObject(ctx, att.ObjectKey)
	if err != nil {
		return fmt.Errorf("read attachment %d (%s): %w", att.ID, att.ObjectKey, err)
	}
	var buf bytes.Buffer
	mw := multipart.NewWriter(&buf)
	_ = mw.WriteField("id", workID)
	_ = mw.WriteField("title", title)
	_ = mw.WriteField("artist", artist)
	_ = mw.WriteField("year", year)
	_ = mw.WriteField("category", category)
	_ = mw.WriteField("aspect", fmt.Sprintf("%.2f", aspect))
	_ = mw.WriteField("visibility", visibility)
	_ = mw.WriteField("description", desc)
	_ = mw.WriteField("curator_note", desc)
	_ = mw.WriteField("alt", title)
	if loc != "" {
		_ = mw.WriteField("location", loc)
	}

	filename := strings.TrimSpace(att.OriginalFilename)
	if filename == "" {
		filename = filepath.Base(att.ObjectKey)
	}
	mimeType := strings.TrimSpace(att.MimeType)
	if mimeType == "" {
		mimeType = "image/jpeg"
	}
	partHeader := make(textproto.MIMEHeader)
	partHeader.Set("Content-Disposition", fmt.Sprintf(`form-data; name="file"; filename=%q`, filename))
	partHeader.Set("Content-Type", mimeType)
	fw, err := mw.CreatePart(partHeader)
	if err != nil {
		return err
	}
	if _, err := fw.Write(rawBytes); err != nil {
		return err
	}
	_ = mw.Close()

	req, err := http.NewRequestWithContext(ctx, http.MethodPost, baseURL+"/api/v1/works", &buf)
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", mw.FormDataContentType())
	req.Header.Set("Authorization", "Bearer "+bridgeToken)
	req.Header.Set("X-Bridge-Token", bridgeToken)
	resp, err := spaceSyncHTTPClient.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	raw, _ := io.ReadAll(io.LimitReader(resp.Body, 32*1024))
	if resp.StatusCode >= 200 && resp.StatusCode < 300 {
		return nil
	}
	return fmt.Errorf("space multipart upload HTTP %d: %s", resp.StatusCode, strings.TrimSpace(string(raw)))
}

// SyncEntryToVesperSpace 将单条《散页》带图记录同步投影到 Vesper Space（支持新增、修改、私密状态切换与移除附件清理）。
func SyncEntryToVesperSpace(ctx context.Context, entryID uint, removedAttIDs []uint) error {
	baseURL, bridgeToken, ok := getVesperSpaceSyncConfig()
	if !ok || db.DB == nil {
		return nil
	}

	for _, remID := range removedAttIDs {
		if err := deleteSpaceWorkByAttachmentID(ctx, baseURL, bridgeToken, remID, true); err != nil {
			log.Printf("[space-sync] remove unlinked attachment %d failed: %v", remID, err)
		}
	}

	if entryID == 0 {
		return nil
	}

	var entry models.LifeEntry
	if err := db.DB.Unscoped().First(&entry, "id = ?", entryID).Error; err != nil {
		return nil
	}

	var atts []models.Attachment
	db.DB.Where("entry_id = ?", entryID).Order("id ASC").Find(&atts)

	if entry.DeletedAt.Valid {
		for _, att := range atts {
			_ = deleteSpaceWorkByAttachmentID(ctx, baseURL, bridgeToken, att.ID, true)
		}
		return nil
	}

	projectable := make([]models.Attachment, 0, len(atts))
	for _, att := range atts {
		if isProjectableAttachment(att) {
			projectable = append(projectable, att)
		}
	}
	if len(projectable) == 0 {
		return nil
	}

	artist := resolveCuratorArtistName()
	var firstErr error
	for idx, att := range projectable {
		if err := projectSingleAttachmentToSpace(ctx, baseURL, bridgeToken, artist, entry, att, idx, len(projectable)); err != nil {
			log.Printf("[space-sync] project entry=%d att=%d failed: %v", entry.ID, att.ID, err)
			if firstErr == nil {
				firstErr = err
			}
		}
	}
	return firstErr
}

// SyncEntryToVesperSpaceAsync 后台异步触发单条《散页》向 Vesper Space 的数据层投影。
func SyncEntryToVesperSpaceAsync(entryID uint, removedAttIDs []uint) {
	if !VesperSpaceProjectionEnabled() {
		return
	}
	go func() {
		ctx, cancel := context.WithTimeout(context.Background(), 90*time.Second)
		defer cancel()
		_ = SyncEntryToVesperSpace(ctx, entryID, removedAttIDs)
	}()
}

// DeleteSpaceWorksForAttachmentsAsync 后台异步从 Vesper Space 撤下已删除散页对应的展品。
func DeleteSpaceWorksForAttachmentsAsync(attIDs []uint, keepMedia bool) {
	baseURL, bridgeToken, ok := getVesperSpaceSyncConfig()
	if !ok || len(attIDs) == 0 {
		return
	}
	go func() {
		ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
		defer cancel()
		for _, id := range attIDs {
			if err := deleteSpaceWorkByAttachmentID(ctx, baseURL, bridgeToken, id, keepMedia); err != nil {
				log.Printf("[space-sync] delete work for att=%d failed: %v", id, err)
			}
		}
	}()
}

// StartVesperSpaceReconciler 启动时后台扫描现有未删除的带图《散页》，自动对齐投影至 Vesper Space 与 5TB 云盘。
func StartVesperSpaceReconciler() {
	if !VesperSpaceProjectionEnabled() {
		return
	}
	go func() {
		time.Sleep(3 * time.Second)
		if db.DB == nil {
			return
		}
		var entryIDs []uint
		if err := db.DB.Model(&models.Attachment{}).
			Where("entry_id IS NOT NULL AND entry_id > 0").
			Distinct("entry_id").
			Pluck("entry_id", &entryIDs).Error; err != nil || len(entryIDs) == 0 {
			return
		}
		log.Printf("[space-sync] reconciling %d existing photo entries to Vesper Space...", len(entryIDs))
		for _, eid := range entryIDs {
			ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
			_ = SyncEntryToVesperSpace(ctx, eid, nil)
			cancel()
		}
	}()
}
