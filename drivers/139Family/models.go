package family139

import (
	"encoding/json"
	"strings"
	"time"

	"litepan/internal/domain"
)

type flexString = json.Number

type apiEnvelope struct {
	Success *bool           `json:"success"`
	Code    flexString      `json:"code"`
	Message string          `json:"message"`
	Data    json.RawMessage `json:"data"`
}

type routePolicyData struct {
	RoutePolicyList []routePolicyItem `json:"routePolicyList"`
}

type routePolicyItem struct {
	ModName  string `json:"modName"`
	HTTPSURL string `json:"httpsUrl"`
	HTTPURL  string `json:"httpUrl"`
}

type familyListData struct {
	CloudCatalogList []familyCatalogEntry `json:"cloudCatalogList"`
	CloudContentList []familyContentEntry `json:"cloudContentList"`
	Path             string               `json:"path"`
	TotalCount       int                  `json:"totalCount"`
}

type familyCatalogEntry struct {
	CatalogID      string `json:"catalogID"`
	CatalogName    string `json:"catalogName"`
	LastUpdateTime string `json:"lastUpdateTime"`
}

type familyContentEntry struct {
	ContentID    string `json:"contentID"`
	ContentName  string `json:"contentName"`
	ContentSize  int64  `json:"contentSize"`
	ThumbnailURL string `json:"thumbnailURL"`
}

func (e familyCatalogEntry) toFileItem(path string) domain.FileItem {
	return domain.FileItem{
		ID:      e.CatalogID,
		Name:    e.CatalogName,
		IsDir:   true,
		ModTime: parseFamilyTime(e.LastUpdateTime),
		IDKind:  domain.IDStable,
	}
}

func (e familyContentEntry) toFileItem(path string) domain.FileItem {
	return domain.FileItem{
		ID:     e.ContentID,
		Name:   e.ContentName,
		Size:   e.ContentSize,
		IsDir:  false,
		Thumb:  e.ThumbnailURL,
		IDKind: domain.IDStable,
	}
}

type downloadURLData struct {
	DownloadURL string `json:"downloadURL"`
}

type batchOprTaskData struct {
	Result struct {
		ResultCode string `json:"resultCode"`
	} `json:"result"`
}

type createDocData struct {
	DocLibID   string `json:"docLibID"`
	DocLibName string `json:"docLibName"`
}

type modifyContentData struct {
	Result struct {
		ResultCode string `json:"resultCode"`
	} `json:"result"`
}

type catalogInfo struct {
	CatalogID string `json:"catalogID"`
	Path      string `json:"path"`
}

type contentInfo struct {
	ContentID string `json:"contentID"`
	Path      string `json:"path"`
}

type refreshTokenResponse struct {
	Return      string `xml:"return"`
	Token       string `xml:"token"`
	AccessToken string `xml:"accessToken"`
	ExpireTime  string `xml:"expiretime"`
	Desc        string `xml:"desc"`
}

func parseFamilyTime(value string) time.Time {
	value = strings.TrimSpace(value)
	if value == "" {
		return time.Time{}
	}
	for _, layout := range []string{time.RFC3339Nano, time.RFC3339, "2006-01-02 15:04:05", "2006-01-02T15:04:05"} {
		if parsed, err := time.Parse(layout, value); err == nil {
			return parsed
		}
	}
	if millis, err := parseInt64(value); err == nil {
		return time.UnixMilli(millis)
	}
	return time.Time{}
}
