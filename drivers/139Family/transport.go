package family139

import (
	"bytes"
	"context"
	"crypto/md5"
	"crypto/rand"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"net/http"
	"net/url"
	"sort"
	"strconv"
	"strings"
	"time"

	"litepan/internal/domain"
	"litepan/internal/driver"
	"litepan/internal/httpx"
)

const (
	routePolicyURL  = "https://user-njs.yun.139.com/user/route/qryRoutePolicy"
	tokenRefreshURL = "https://aas.caiyun.feixin.10086.cn/tellin/authTokenRefresh.do"
	webOrigin       = "https://yun.139.com"
	userAgent       = "Mozilla/5.0 (Windows NT 10.0; Win64; x64) AppleWebKit/537.36 (KHTML, like Gecko) Chrome/120.0.0.0 Safari/537.36"

	familyQueryContentList = "/orchestration/familyCloud-rebuild/content/v1.2/queryContentList"
	familyGetDownloadURL   = "/orchestration/familyCloud-rebuild/content/v1.0/getFileDownLoadURL"
	familyCreateCloudDoc   = "/orchestration/familyCloud-rebuild/cloudCatalog/v1.0/createCloudDoc"
	familyBatchOprTask     = "/orchestration/familyCloud-rebuild/batchOprTask/v1.0/createBatchOprTask"
	familyModifyContent    = "/orchestration/familyCloud-rebuild/andAlbum/openApi/modifyContentInfo"
	isboBatchOprTask       = "/isbo/openApi/createBatchOprTask"

	listPageSize            = 100
	defaultOperationDelayMS = 300
)

func (d *Driver) rootID() string {
	if root := strings.TrimSpace(d.add.RootFolderID); root != "" {
		return root
	}
	return d.providerRoot
}

func (d *Driver) normalizeParent(parentID string) string {
	p := strings.TrimSpace(parentID)
	if p == "" || p == "0" || p == "root" || p == "/" {
		return d.rootID()
	}
	id, _ := splitFamilyItemID(p)
	return id
}

func (d *Driver) waitOperationDelay(ctx context.Context) error {
	return driver.WaitRequestInterval(ctx, d.intervalGate, defaultOperationDelayMS)
}

// queryRoutePolicy 从路由策略查询指定 modName 的主机地址。
func (d *Driver) queryRoutePolicy(ctx context.Context, modName string) (string, error) {
	body := map[string]any{
		"userInfo": map[string]any{
			"userType":    1,
			"accountType": 1,
			"accountName": d.currentAccount(),
		},
		"modAddrType": 1,
	}
	var route routePolicyData
	err := d.signedRequest(ctx, routePolicyURL, signScopeFamily, body, &route)
	if isAuthError(err) {
		if _, refreshErr := d.refreshAuthorization(ctx, true); refreshErr != nil {
			return "", refreshErr
		}
		err = d.signedRequest(ctx, routePolicyURL, signScopeFamily, body, &route)
	}
	if err != nil {
		return "", err
	}
	for _, item := range route.RoutePolicyList {
		if strings.EqualFold(strings.TrimSpace(item.ModName), modName) {
			host := strings.TrimRight(firstNonEmpty(item.HTTPSURL, item.HTTPURL), "/")
			if host != "" {
				return host, nil
			}
		}
	}
	return "", domain.Errorf(domain.CodeDriverError, "移动家庭云路由策略未返回 %s 主机", modName)
}

// queryFamilyCloudHost 查询 GroupCloudHost（家庭云主操作：列表、下载、删除等）。
func (d *Driver) queryFamilyCloudHost(ctx context.Context) (string, error) {
	return d.queryRoutePolicy(ctx, "group")
}

// queryAndAlbumHost 查询 AndAlbum 主机（家庭云移动、复制、重命名用）。
func (d *Driver) queryAndAlbumHost(ctx context.Context) (string, error) {
	return d.queryRoutePolicy(ctx, "family")
}

type signScope int

const (
	signScopePersonal signScope = iota
	signScopeFamily
)

// signedRequest 发送带签名的 POST JSON 请求。
func (d *Driver) signedRequest(ctx context.Context, rawURL string, scope signScope, body, out any) error {
	if err := d.waitOperationDelay(ctx); err != nil {
		return err
	}
	rawBody, err := json.Marshal(body)
	if err != nil {
		return domain.Wrap(domain.CodeInternal, err)
	}
	ts := time.Now().Format("2006-01-02 15:04:05")
	randomValue, err := randomString(16)
	if err != nil {
		return domain.Wrap(domain.CodeInternal, err)
	}
	headers := d.signedHeaders(d.currentAuthorization(), ts, randomValue, calcSign(string(rawBody), ts, randomValue), scope)
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, rawURL, bytes.NewReader(rawBody))
	if err != nil {
		return domain.Wrap(domain.CodeInternal, err)
	}
	for key, value := range headers {
		req.Header.Set(key, value)
	}
	resp, data, err := httpx.Execute(d.client, req, httpx.DefaultReadLimit)
	if err != nil {
		return domain.Wrap(domain.CodeDriverError, err)
	}
	if resp.StatusCode == http.StatusUnauthorized {
		return domain.Errorf(domain.CodeAuthExpired, "移动家庭云认证已过期")
	}
	if resp.StatusCode == http.StatusForbidden {
		return domain.Errorf(domain.CodePermissionDenied, "移动家庭云拒绝访问")
	}
	if resp.StatusCode < http.StatusOK || resp.StatusCode >= http.StatusMultipleChoices {
		return domain.Errorf(domain.CodeDriverError, "移动家庭云 API HTTP %d: %s", resp.StatusCode, httpx.Truncate(data, 300))
	}
	var envelope apiEnvelope
	if err := json.Unmarshal(data, &envelope); err != nil {
		return domain.Wrap(domain.CodeDriverError, err)
	}
	// CreateBatchOprTaskResp 的 "成功"判定标准不同：resultCode == "0"
	if isBatchOpResponse(envelope, out) {
		return nil
	}
	if envelope.Success != nil && !*envelope.Success {
		return mapAPIError(envelope.Code.String(), envelope.Message)
	}
	if out != nil && len(envelope.Data) > 0 && string(envelope.Data) != "null" {
		if err := json.Unmarshal(envelope.Data, out); err != nil {
			return domain.Wrap(domain.CodeDriverError, err)
		}
	}
	return nil
}

// isBatchOpResponse 检查是否是批量操作响应且 resultCode == "0"。
func isBatchOpResponse(env apiEnvelope, out any) bool {
	if out == nil {
		return false
	}
	resp, ok := out.(*batchOprTaskData)
	if !ok {
		return false
	}
	// 尝试从 data 字段解析
	if len(env.Data) > 0 && string(env.Data) != "null" {
		var result struct {
			Result struct {
				ResultCode string `json:"resultCode"`
			} `json:"result"`
		}
		if err := json.Unmarshal(env.Data, &result); err == nil && result.Result.ResultCode == "0" {
			resp.Result.ResultCode = result.Result.ResultCode
			return true
		}
	}
	return false
}

// signedHeaders 返回家庭云常规 API（列表、下载、删除等）的签名请求头。
// 对齐 OpenList 的 request() —— 不包含 x-yun-* 系列和 Caller/mcloud-route。
func (d *Driver) signedHeaders(authorization, ts, randomValue, sign string, scope signScope) map[string]string {
	svcType := "1"
	if scope == signScopeFamily {
		svcType = "2"
	}
	return map[string]string{
		"Accept":                 "application/json, text/plain, */*",
		"Authorization":          "Basic " + normalizeAuthorization(authorization),
		"CMS-DEVICE":             "default",
		"Content-Type":           "application/json",
		"Inner-Hcy-Router-Https": "1",
		"mcloud-channel":         "1000101",
		"mcloud-client":          "10701",
		"mcloud-sign":            ts + "," + randomValue + "," + sign,
		"mcloud-version":         "7.14.0",
		"Origin":                 webOrigin,
		"Referer":                webOrigin + "/w/",
		"User-Agent":             userAgent,
		"x-DeviceInfo":           "||9|7.14.0|chrome|120.0.0.0|||windows 10||zh-CN|||",
		"x-huawei-channelSrc":    "10000034",
		"x-inner-ntwk":           "2",
		"x-m4c-caller":           "PC",
		"x-m4c-src":              "10002",
		"x-SvcType":              svcType,
	}
}

// uploadSignedHeaders 返回家庭云上传 API（/dynamic/file/*）的签名请求头。
// 对齐 OpenList 的 newRequest() —— 额外包含 x-yun-* 系列和 Caller/mcloud-route。
func (d *Driver) uploadSignedHeaders(authorization, ts, randomValue, sign string, scope signScope) map[string]string {
	svcType := "1"
	if scope == signScopeFamily {
		svcType = "2"
	}
	return map[string]string{
		"Accept":                 "application/json, text/plain, */*",
		"Authorization":          "Basic " + normalizeAuthorization(authorization),
		"CMS-DEVICE":             "default",
		"Content-Type":           "application/json",
		"Inner-Hcy-Router-Https": "1",
		"Caller":                 "web",
		"mcloud-channel":         "1000101",
		"mcloud-client":          "10701",
		"mcloud-route":           "001",
		"mcloud-sign":            ts + "," + randomValue + "," + sign,
		"mcloud-version":         "7.14.0",
		"Origin":                 webOrigin,
		"Referer":                webOrigin + "/w/",
		"User-Agent":             userAgent,
		"x-DeviceInfo":           "||9|7.14.0|chrome|120.0.0.0|||windows 10||zh-CN|||",
		"x-huawei-channelSrc":    "10000034",
		"x-inner-ntwk":           "2",
		"x-m4c-caller":           "PC",
		"x-m4c-src":              "10002",
		"x-SvcType":              svcType,
		"x-yun-api-version":      "v1",
		"x-yun-app-channel":      "10000034",
		"x-yun-channel-source":   "10000034",
		"x-yun-client-info":      "||9|7.14.0|chrome|120.0.0.0|||windows 10||zh-CN|||dW5kZWZpbmVk||",
		"x-yun-module-type":      "100",
		"x-yun-svc-type":         svcType,
	}
}

// signedUploadRequest 发送带签名的 POST JSON 请求，使用上传专用请求头（x-yun-* 系列）。
func (d *Driver) signedUploadRequest(ctx context.Context, rawURL string, scope signScope, body, out any) error {
	if err := d.waitOperationDelay(ctx); err != nil {
		return err
	}
	rawBody, err := json.Marshal(body)
	if err != nil {
		return domain.Wrap(domain.CodeInternal, err)
	}
	ts := time.Now().Format("2006-01-02 15:04:05")
	randomValue, err := randomString(16)
	if err != nil {
		return domain.Wrap(domain.CodeInternal, err)
	}
	headers := d.uploadSignedHeaders(d.currentAuthorization(), ts, randomValue, calcSign(string(rawBody), ts, randomValue), scope)
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, rawURL, bytes.NewReader(rawBody))
	if err != nil {
		return domain.Wrap(domain.CodeInternal, err)
	}
	for key, value := range headers {
		req.Header.Set(key, value)
	}
	resp, data, err := httpx.Execute(d.client, req, httpx.DefaultReadLimit)
	if err != nil {
		return domain.Wrap(domain.CodeDriverError, err)
	}
	if resp.StatusCode == http.StatusUnauthorized {
		return domain.Errorf(domain.CodeAuthExpired, "移动家庭云认证已过期")
	}
	if resp.StatusCode == http.StatusForbidden {
		return domain.Errorf(domain.CodePermissionDenied, "移动家庭云拒绝访问")
	}
	if resp.StatusCode < http.StatusOK || resp.StatusCode >= http.StatusMultipleChoices {
		return domain.Errorf(domain.CodeDriverError, "移动家庭云 API HTTP %d: %s", resp.StatusCode, httpx.Truncate(data, 300))
	}
	var envelope apiEnvelope
	if err := json.Unmarshal(data, &envelope); err != nil {
		return domain.Wrap(domain.CodeDriverError, err)
	}
	if isBatchOpResponse(envelope, out) {
		return nil
	}
	if envelope.Success != nil && !*envelope.Success {
		return mapAPIError(envelope.Code.String(), envelope.Message)
	}
	if out != nil && len(envelope.Data) > 0 && string(envelope.Data) != "null" {
		if err := json.Unmarshal(envelope.Data, out); err != nil {
			return domain.Wrap(domain.CodeDriverError, err)
		}
	}
	return nil
}

// familyUploadAPIRequest 是家庭云上传 API 的统一入口，使用上传专用请求头。
func (d *Driver) familyUploadAPIRequest(ctx context.Context, path string, body map[string]any, out any) error {
	if d.groupHost == "" {
		return domain.Errorf(domain.CodeDriverError, "移动家庭云尚未获取 API 主机地址")
	}
	enriched := d.familyNewJson(body)
	err := d.signedUploadRequest(ctx, d.groupHost+path, signScopeFamily, enriched, out)
	if !isAuthError(err) {
		return err
	}
	if _, refreshErr := d.refreshAuthorization(ctx, true); refreshErr != nil {
		return refreshErr
	}
	return d.signedUploadRequest(ctx, d.groupHost+path, signScopeFamily, enriched, out)
}

// familyAPIRequest 是家庭云 API 的统一入口：注入通用参数 → 签名 → 请求 → 自动刷新重试。
// 对齐 OpenList 的 post() —— 使用固定域名 yun.139.com（列表/下载/删除等主操作）。
func (d *Driver) familyAPIRequest(ctx context.Context, path string, body map[string]any, out any) error {
	enriched := d.familyNewJson(body)
	err := d.signedRequest(ctx, webOrigin+path, signScopeFamily, enriched, out)
	if !isAuthError(err) {
		return err
	}
	if _, refreshErr := d.refreshAuthorization(ctx, true); refreshErr != nil {
		return refreshErr
	}
	return d.signedRequest(ctx, webOrigin+path, signScopeFamily, enriched, out)
}

// familyAndAlbumRequest 是 AndAlbum 接口（移动/复制/重命名）的统一入口。
// 使用 FamilyCloudHost（AndAlbum 主机）。
func (d *Driver) familyAndAlbumRequest(ctx context.Context, path string, body map[string]any, out any) error {
	if d.familyHost == "" {
		return domain.Errorf(domain.CodeDriverError, "移动家庭云尚未获取 AndAlbum 主机地址")
	}
	enriched := d.familyNewJson(body)
	err := d.signedRequest(ctx, d.familyHost+path, signScopeFamily, enriched, out)
	if !isAuthError(err) {
		return err
	}
	if _, refreshErr := d.refreshAuthorization(ctx, true); refreshErr != nil {
		return refreshErr
	}
	return d.signedRequest(ctx, d.familyHost+path, signScopeFamily, enriched, out)
}

// isboAPIRequest 是 ISBO 接口（移动/复制）的统一入口。
func (d *Driver) isboAPIRequest(ctx context.Context, path string, body map[string]any, out any) error {
	err := d.signedRequest(ctx, isboBaseURL+path, signScopeFamily, body, out)
	if !isAuthError(err) {
		return err
	}
	if _, refreshErr := d.refreshAuthorization(ctx, true); refreshErr != nil {
		return refreshErr
	}
	return d.signedRequest(ctx, isboBaseURL+path, signScopeFamily, body, out)
}

const isboBaseURL = "https://group.yun.139.com/hcy/mutual/adapter"

// familyNewJson 在请求体中注入家庭云通用参数。
func (d *Driver) familyNewJson(data map[string]any) map[string]any {
	result := make(map[string]any, len(data)+4)
	for k, v := range data {
		result[k] = v
	}
	result["catalogType"] = 3
	result["cloudID"] = d.cloudID
	result["cloudType"] = 1
	result["commonAccountInfo"] = map[string]any{
		"account":     d.currentAccount(),
		"accountType": 1,
	}
	return result
}

func calcSign(body, ts, randomValue string) string {
	encoded := encodeURIComponent(body)
	chars := strings.Split(encoded, "")
	sort.Strings(chars)
	sortedBody := strings.Join(chars, "")
	encodedBody := base64.StdEncoding.EncodeToString([]byte(sortedBody))
	first := md5.Sum([]byte(encodedBody))
	second := md5.Sum([]byte(ts + ":" + randomValue))
	final := md5.Sum([]byte(hex.EncodeToString(first[:]) + hex.EncodeToString(second[:])))
	return strings.ToUpper(hex.EncodeToString(final[:]))
}

func encodeURIComponent(value string) string {
	encoded := url.QueryEscape(value)
	return strings.ReplaceAll(encoded, "+", "%20")
}

func randomString(length int) (string, error) {
	const alphabet = "abcdefghijklmnopqrstuvwxyzABCDEFGHIJKLMNOPQRSTUVWXYZ0123456789"
	buf := make([]byte, length)
	if _, err := rand.Read(buf); err != nil {
		return "", err
	}
	for index := range buf {
		buf[index] = alphabet[int(buf[index])%len(alphabet)]
	}
	return string(buf), nil
}

func mapAPIError(code, message string) error {
	message = strings.TrimSpace(message)
	if message == "" {
		message = "移动家庭云 API 返回错误"
	}
	switch strings.TrimSpace(code) {
	case "9000", "9008", "9100", "100002":
		return domain.Errorf(domain.CodeAuthExpired, "移动家庭云认证已过期：%s", message)
	case "403", "100403":
		return domain.Errorf(domain.CodePermissionDenied, "移动家庭云权限不足：%s", message)
	case "429":
		return domain.Errorf(domain.CodeRateLimited, "移动家庭云接口限流：%s", message)
	default:
		return domain.Errorf(domain.CodeDriverError, "移动家庭云 API 错误(%s)：%s", code, message)
	}
}

func isAuthError(err error) bool {
	ae, ok := domain.AsAppError(err)
	return ok && ae.Code == domain.CodeAuthExpired
}

func parseInt64(value string) (int64, error) {
	return strconv.ParseInt(strings.TrimSpace(value), 10, 64)
}

// splitFamilyItemID 从编码的文件 ID 中分离 contentID/catalogID 和父目录路径。
// 编码格式：contentID|parentPath；无 | 时 path 为空。
func splitFamilyItemID(fileID string) (id, path string) {
	id, path, _ = strings.Cut(fileID, "|")
	return
}

func encodeFamilyItemID(id, parentPath string) string {
	if parentPath == "" {
		return id
	}
	return id + "|" + parentPath
}

func firstNonEmpty(values ...string) string {
	for _, value := range values {
		if value = strings.TrimSpace(value); value != "" {
			return value
		}
	}
	return ""
}
