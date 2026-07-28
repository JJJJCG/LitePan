package family139

import (
	"context"
	"net/http"
	"strings"
	"sync"
	"time"

	"litepan/internal/domain"
	"litepan/internal/driver"
	"litepan/internal/httpx"
)

type Driver struct {
	add    Addition
	client *http.Client

	intervalGate driver.RequestIntervalGate
	persist      driver.AuthPersistFunc

	mu        sync.RWMutex
	refreshMu sync.Mutex

	authorization string
	account       string
	familyHost    string
	cloudID       string
	providerRoot  string
}

var config = driver.Config{
	Name:                   "139_family",
	DisplayName:            "移动家庭云",
	Description:            "中国移动 139 家庭云，使用网页 Authorization 令牌",
	CardTags:               []string{"Authorization", "支持302"},
	SortOrder:              8,
	AuthLabel:              "Authorization",
	CardColor:              "#3B82F6",
	CardLogo:               "/logos/yidong.png",
	DefaultRoot:            "",
	AuthType:               driver.AuthToken,
	TokenLifetime:          30 * 24 * time.Hour,
	RefreshAdvance:         10 * time.Hour,
	UploadConflictPolicies: []string{"overwrite", "rename", "skip", "fail"},
}

func New() driver.Driver { return &Driver{} }

func init() { driver.Register(New) }

func (d *Driver) Config() driver.Config { return config }

func (d *Driver) GetAddition() any { return &d.add }

func (d *Driver) SetAuthCredentials(creds domain.AuthCredentials) {
	d.mu.Lock()
	defer d.mu.Unlock()
	d.authorization = normalizeAuthorization(creds.AccessToken)
}

func (d *Driver) SetAuthPersister(fn driver.AuthPersistFunc) { d.persist = fn }

func (d *Driver) SetRequestIntervalGate(gate driver.RequestIntervalGate) { d.intervalGate = gate }

func (d *Driver) Init(ctx context.Context) error {
	if d.client == nil {
		d.client = httpx.NewClient(httpx.ClientOptions{Timeout: 60 * time.Second})
	}

	authorization := d.currentAuthorization()
	if authorization == "" {
		authorization = normalizeAuthorization(d.add.Authorization)
	}
	info, err := parseAuthorization(authorization)
	if err != nil {
		return err
	}
	d.setAuthorization(info)
	d.cloudID = strings.TrimSpace(d.add.CloudID)
	if d.cloudID == "" {
		return domain.Errorf(domain.CodeValidation, "家庭云ID 不能为空")
	}

	if time.Until(info.ExpiresAt) < config.RefreshAdvance {
		if _, err := d.doRefresh(ctx); err != nil {
			return err
		}
	}

	host, err := d.queryFamilyCloudHost(ctx)
	if err != nil {
		return err
	}
	d.familyHost = host

	if d.providerRoot == "" {
		root, err := d.getFamilyRootPath(ctx)
		if err != nil {
			return err
		}
		d.providerRoot = root
	}

	return nil
}

func (d *Driver) Drop(context.Context) error {
	httpx.CloseClient(d.client)
	return nil
}

func (d *Driver) Ping(ctx context.Context) error {
	_, err := d.ListFiles(ctx, d.rootID())
	return err
}

func (d *Driver) ListFiles(ctx context.Context, parentID string) ([]domain.FileItem, error) {
	catalogID := d.normalizeParent(parentID)
	if catalogID == d.rootID() || catalogID == "" {
		catalogID = ""
	}
	pageNum := 1
	items := make([]domain.FileItem, 0)
	for {
		var data familyListData
		if err := d.familyAPIRequest(ctx, familyQueryContentList, map[string]any{
			"catalogID":       catalogID,
			"pageInfo":        map[string]any{"pageNum": pageNum, "pageSize": listPageSize},
			"sortDirection":   1,
			"contentSortType": 0,
		}, &data); err != nil {
			return nil, err
		}
		for _, cat := range data.CloudCatalogList {
			items = append(items, cat.toFileItem(data.Path))
		}
		for _, con := range data.CloudContentList {
			items = append(items, con.toFileItem(data.Path))
		}
		if data.TotalCount == 0 || len(items) >= data.TotalCount {
			break
		}
		pageNum++
	}
	return items, nil
}

func (d *Driver) GetFileInfo(ctx context.Context, fileID string) (*domain.FileItem, error) {
	fileID = strings.TrimSpace(fileID)
	root := d.rootID()
	if fileID == "" || fileID == "0" || fileID == "root" || fileID == "/" || fileID == root {
		return &domain.FileItem{
			ID:     root,
			Name:   "根目录",
			IsDir:  true,
			IDKind: domain.IDStable,
		}, nil
	}
	// 家庭云没有单独的文件信息接口，复用列表接口查询父目录
	// 这里通过简单的列表重新查找的方式来获取单个文件信息
	// 对于文件的 GetFileInfo，在 LitePan 中通常通过缓存或父目录列表解决
	return nil, domain.Errorf(domain.CodeNotImplement, "移动家庭云暂不支持单文件查询，请使用列表操作")
}

func (d *Driver) ExplainConnectionError(technical string, saving bool) string {
	prefix := "添加失败"
	if saving {
		prefix = "保存失败"
	}
	lower := strings.ToLower(technical)
	switch {
	case strings.Contains(technical, "Authorization 令牌不能为空"), strings.Contains(technical, "Authorization 令牌格式"):
		return prefix + "：请在移动家庭云网页请求头中复制完整 Authorization 值"
	case strings.Contains(lower, "auth_expired"), strings.Contains(technical, "认证已过期"):
		return prefix + "：移动家庭云 Authorization 已失效，请重新从网页抓取"
	case strings.Contains(technical, "家庭云ID 不能为空"):
		return prefix + "：请填写家庭云ID"
	case strings.Contains(technical, "移动家庭云路由策略"):
		return prefix + "：获取家庭云服务地址失败，" + technical
	default:
		return ""
	}
}

// getFamilyRootPath 查询家庭云根目录路径。
func (d *Driver) getFamilyRootPath(ctx context.Context) (string, error) {
	var data familyListData
	if err := d.familyAPIRequest(ctx, familyQueryContentList, map[string]any{
		"catalogID":       "",
		"pageInfo":        map[string]any{"pageNum": 1, "pageSize": 1},
		"sortDirection":   1,
		"contentSortType": 0,
	}, &data); err != nil {
		return "", err
	}
	path := strings.TrimPrefix(data.Path, "root:/")
	return path, nil
}

// familyDirPath 根据对象路径拼接完整目录路径。
func (d *Driver) familyDirPath(objPath, objID string) string {
	base := strings.TrimRight(objPath, "/")
	if base == "" {
		return objID
	}
	if objID == "" || objID == "/" {
		return base
	}
	return base + "/" + objID
}

func (d *Driver) currentAuthorization() string {
	d.mu.RLock()
	defer d.mu.RUnlock()
	return d.authorization
}

func (d *Driver) currentAccount() string {
	d.mu.RLock()
	defer d.mu.RUnlock()
	return d.account
}

func (d *Driver) setAuthorization(info authorizationInfo) {
	d.mu.Lock()
	d.authorization = info.Authorization
	d.account = info.Account
	d.mu.Unlock()
}

var (
	_ driver.Driver                   = (*Driver)(nil)
	_ driver.InfoGetter               = (*Driver)(nil)
	_ driver.Downloader               = (*Driver)(nil)
	_ driver.Deleter                  = (*Driver)(nil)
	_ driver.Mover                    = (*Driver)(nil)
	_ driver.Copier                   = (*Driver)(nil)
	_ driver.Renamer                  = (*Driver)(nil)
	_ driver.FolderCreator            = (*Driver)(nil)
	_ driver.LocalUploader            = (*Driver)(nil)
	_ driver.AuthRefresher            = (*Driver)(nil)
	_ driver.AuthCredentialConsumer   = (*Driver)(nil)
	_ driver.AuthPersistConsumer      = (*Driver)(nil)
	_ driver.ConnectionErrorExplainer = (*Driver)(nil)
	_ driver.RequestIntervalConsumer  = (*Driver)(nil)
)
