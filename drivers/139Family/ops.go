package family139

import (
	"context"
	"net/http"
	"strings"
	"time"

	"litepan/internal/domain"
	"litepan/internal/driver"
	"litepan/internal/driver/uploadutil"
)

const (
	downloadPartSize    = 10 * 1024 * 1024
	downloadConcurrency = 3
)

func (d *Driver) ResolveDownload(ctx context.Context, req driver.DownloadRequest) (*domain.DownloadInfo, error) {
	contentID, parentPath := splitFamilyItemID(strings.TrimSpace(req.FileID))
	if contentID == "" {
		return nil, domain.Errorf(domain.CodeValidation, "content_id 不能为空")
	}

	var data downloadURLData
	if err := d.familyAPIRequest(ctx, familyGetDownloadURL, map[string]any{
		"contentID": contentID,
		"path":      parentPath,
	}, &data); err != nil {
		return nil, err
	}
	if data.DownloadURL == "" {
		return nil, domain.Errorf(domain.CodeDriverError, "移动家庭云未返回下载链接")
	}

	headers := http.Header{}
	headers.Set("User-Agent", firstNonEmpty(req.UA, userAgent))
	headers.Set("Referer", webOrigin+"/")
	headers.Set("Origin", webOrigin)

	mode := domain.DownloadRedirect
	forceProxy := false
	if strings.EqualFold(strings.TrimSpace(d.add.DownloadMode), "proxy") {
		mode = domain.DownloadProxy
		forceProxy = true
	}

	return &domain.DownloadInfo{
		URL:         data.DownloadURL,
		Headers:     headers,
		Mode:        mode,
		ForceProxy:  forceProxy,
		Expiration:  30 * time.Minute,
		ChunkSize:   downloadPartSize,
		Concurrency: downloadConcurrency,
	}, nil
}

func (d *Driver) CreateFolder(ctx context.Context, parentID, name string) (*domain.FileItem, error) {
	name = strings.TrimSpace(name)
	if name == "" {
		return nil, domain.Errorf(domain.CodeValidation, "文件夹名称不能为空")
	}
	if err := uploadutil.ValidateFileName(name); err != nil {
		return nil, err
	}

	var data createDocData
	if err := d.familyAPIRequest(ctx, familyCreateCloudDoc, map[string]any{
		"docLibName": name,
		"path":       d.familyDirPath("", d.normalizeParent(parentID)),
	}, &data); err != nil {
		return nil, err
	}
	folderName := firstNonEmpty(data.DocLibName, name)
	return &domain.FileItem{
		ID:     data.DocLibID,
		Name:   folderName,
		IsDir:  true,
		IDKind: domain.IDStable,
	}, nil
}

func (d *Driver) DeleteFiles(ctx context.Context, fileIDs []string) error {
	if len(fileIDs) == 0 {
		return nil
	}
	catalogList, contentList := d.splitCatalogContent(fileIDs)
	if len(catalogList) == 0 && len(contentList) == 0 {
		return nil
	}
	var result batchOprTaskData
	return d.familyAPIRequest(ctx, familyBatchOprTask, map[string]any{
		"catalogList": catalogList,
		"contentList": contentList,
		"taskType":    2,
		"path":        "",
	}, &result)
}

func (d *Driver) MoveFiles(ctx context.Context, fileIDs []string, targetParentID, sourceParentID string) error {
	ids := d.normalizeFileIDs(fileIDs)
	if len(ids) == 0 {
		return nil
	}
	catalogList, contentList := d.splitCatalogContent(ids)
	destPath := d.familyDirPath("", d.normalizeParent(targetParentID))
	var result batchOprTaskData
	return d.isboAPIRequest(ctx, isboBatchOprTask, map[string]any{
		"catalogList":   catalogList,
		"contentList":   contentList,
		"destCatalogID": d.normalizeParent(targetParentID),
		"destGroupID":   d.cloudID,
		"destPath":      destPath,
		"srcGroupID":    d.cloudID,
		"taskType":      3,
		"accountInfo": map[string]any{
			"accountName": d.currentAccount(),
			"accountType": "1",
		},
	}, &result)
}

func (d *Driver) CopyFiles(ctx context.Context, fileIDs []string, targetParentID string) error {
	ids := d.normalizeFileIDs(fileIDs)
	if len(ids) == 0 {
		return nil
	}
	catalogList, contentList := d.splitCatalogContent(ids)
	destPath := d.familyDirPath("", d.normalizeParent(targetParentID))
	var result batchOprTaskData
	return d.isboAPIRequest(ctx, isboBatchOprTask, map[string]any{
		"catalogList":   catalogList,
		"contentList":   contentList,
		"destCatalogID": d.normalizeParent(targetParentID),
		"destGroupID":   d.cloudID,
		"destPath":      destPath,
		"srcGroupID":    d.cloudID,
		"taskType":      4,
		"accountInfo": map[string]any{
			"accountName": d.currentAccount(),
			"accountType": "1",
		},
	}, &result)
}

func (d *Driver) RenameFile(ctx context.Context, fileID, newName string) error {
	fileID, _ = splitFamilyItemID(strings.TrimSpace(fileID))
	newName = strings.TrimSpace(newName)
	if fileID == "" {
		return domain.Errorf(domain.CodeValidation, "file_id 不能为空")
	}
	if fileID == d.rootID() || fileID == "/" || fileID == "0" {
		return domain.Errorf(domain.CodeValidation, "根目录不支持重命名")
	}
	if newName == "" {
		return domain.Errorf(domain.CodeValidation, "新名称不能为空")
	}
	if err := uploadutil.ValidateFileName(newName); err != nil {
		return err
	}

	var result modifyContentData
	return d.familyAndAlbumRequest(ctx, familyModifyContent, map[string]any{
		"contentID": fileID,
		"newName":   newName,
	}, &result)
}

// splitCatalogContent 将 fileID 列表按文件/目录类型分组（当前简化处理：单一操作都由驱动层判断）。
// 139 家庭云的批量操作需要分别传 catalogList（目录）和 contentList（文件），且每条需要 path。
// 由于 LitePan 的 Delete/Move/Copy 接口只传 fileID 列表，不传路径，
// 这里统一将 ID 作为 contentList 传入，由服务端判断。
func (d *Driver) splitCatalogContent(fileIDs []string) ([]catalogInfo, []contentInfo) {
	catalogs := make([]catalogInfo, 0)
	contents := make([]contentInfo, 0)
	for _, encoded := range fileIDs {
		if encoded == "" {
			continue
		}
		id, path := splitFamilyItemID(encoded)
		// 暂不区分目录/文件类型，统一按 contentList 提交
		contents = append(contents, contentInfo{
			ContentID: id,
			Path:      path,
		})
	}
	return catalogs, contents
}

func (d *Driver) normalizeFileIDs(ids []string) []string {
	result := make([]string, 0, len(ids))
	for _, id := range ids {
		if id = strings.TrimSpace(id); id != "" {
			result = append(result, id)
		}
	}
	return result
}
