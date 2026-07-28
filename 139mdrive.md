# 139 家庭云驱动实现参考手册

> 基于 OpenList v4 的 `drivers/139/` 驱动实现，专注于**令牌认证方式**，供迁移到类似项目时参考。

---

## 一、架构概览

```text
drivers/139/
├── meta.go       # Addition 配置 + Config + init() 注册
├── driver.go     # Yun139 主结构体 + 全部 Driver 接口实现
├── util.go       # 辅助函数：认证、API请求、列表/链接/上传
├── types.go      # API 请求/响应类型定义
└── share_test.go # 分享功能测试
```

**关键设计：一个结构体支持 5 种模式**（通过 `Addition.Type` 区分）：

| 模式 | 值 | 说明 |
|---|---|---|
| 新版个人云 | `"personal_new"` | 新版 API（默认） |
| 旧版个人云 | `"personal"` | 旧版 API |
| **家庭云** | **`"family"`** | **目标模式** |
| 共享群 | `"group"` | 群组云盘 |
| 分享链接 | `"share"` | 外部分享 |

> 迁移时若目标项目只有一种模式，可直接简化，去掉 Type 分支逻辑。

---

## 二、核心数据结构

### 2.1 驱动主结构体

```go
type Yun139 struct {
    model.Storage                          // 内嵌数据库模型（ID, MountPath, Driver等）
    Addition                               // 内嵌用户配置
    cron              *cron.Cron           // 定时刷新 token（每12小时）
    Account           string               // 从 token 解码出的账号
    ref               *Yun139              // 引用其他实例（Reference接口）
    PersonalCloudHost string               // 路由策略 -> API 主机
    FamilyCloudHost   string               // 家庭云 API 主机
    GroupCloudHost    string               // 共享群 API 主机
    ProviderRoot      string               // 家庭云根路径（持久化用）
}
```

**迁移要点：**
- `model.Storage` 是框架内嵌的数据库对象，迁移时替换为目标项目的存储模型
- `ref` 字段实现 `Reference` 接口，支持一个驱动引用另一个同类型驱动（代理模式），非必需可去掉
- 家庭云、共享群各自有独立的 API 主机地址，如果目标项目是单一服务可合并

### 2.2 用户配置 (Addition)

```go
type Addition struct {
    // 令牌认证（二选一）
    Authorization string `json:"authorization" type:"text" required:"true"` // base64编码的令牌
    Username      string `json:"username" required:"true"`                  // 用户名（备用）
    Password      string `json:"password" required:"true" secret:"true"`    // 密码（备用）
    MailCookies   string `json:"mail_cookies" type:"text"`                  // 登录辅助 Cookie

    driver.RootID                           // 内嵌：RootFolderID 根目录ID

    // 业务配置
    Type      string `json:"type" type:"select" options:"personal_new,family,group,personal,share" default:"personal_new"`
    CloudID   string `json:"cloud_id"`                    // 家庭云/共享群的cloudID
    LinkID    string `json:"link_id"`                     // 分享ID

    // 上传/展示选项
    CustomUploadPartSize int64  `json:"custom_upload_part_size" default:"0"`
    UseLargeThumbnail    bool   `json:"use_large_thumbnail" default:"false"`
    UseOldStreamUpload   bool   `json:"use_old_stream_upload" default:"false"`
}
```

**迁移要点：**
- `driver.RootID` 提供 `RootFolderID string` 字段和 `GetRootId()` 方法，也可直接用 `RootFolderID string` 替代
- 字段标签的 `json` 名会被反射读取生成前端配置表单；`required`、`default`、`help`、`secret`、`type` 都是可选的框架特性

### 2.3 驱动配置 (Config)

```go
var config = driver.Config{
    Name:             "139Yun",       // 驱动唯一名称
    LocalSort:        true,           // 本地排序（而非服务端排序）
    ProxyRangeOption: true,           // 支持分片代理下载
}
```

### 2.4 注册入口

```go
func init() {
    op.RegisterDriver(func() driver.Driver {
        d := &Yun139{}
        d.ProxyRange = true
        return d
    })
}
```

> `init()` 在包被导入时自动执行，将构造函数注册到全局 `driverMap`。

---

## 三、令牌认证实现（核心）

### 3.1 令牌格式

`Authorization` 字段是 **base64 编码**的凭证，解码后格式：

```
<固定前缀>:<账号>:<原始Token>|<其他字段>|<其他字段>|<过期时间ms>
```

其中 `<过期时间ms>` 是 Unix 毫秒时间戳。

### 3.2 令牌刷新

```go
func (d *Yun139) refreshToken() error {
    // 1. 解码 base64，提取 Account 和原始 Token
    decode, _ := base64.StdEncoding.DecodeString(d.Authorization)
    splits := strings.Split(string(decode), ":")
    d.Account = splits[1]                     // 提取账号
    strs := strings.Split(splits[2], "|")
    expiration, _ := strconv.ParseInt(strs[3], 10, 64)  // 提取过期时间

    // 2. 判断是否需要刷新（15天内有效不刷新，已过期报错）
    expiration -= time.Now().UnixMilli()
    if expiration > 1000*60*60*24*15 { return nil }  // 无需刷新
    if expiration < 0 { return error }                // 已过期

    // 3. 调用刷新 API
    url := "https://aas.caiyun.feixin.10086.cn:443/tellin/authTokenRefresh.do"
    reqBody := "<root><token>" + token + "</token><account>" + account + "</account><clienttype>656</clienttype></root>"
    resp, _ := base.RestyClient.R().SetBody(reqBody).Post(url)

    // 4. 刷新成功后重新 base64 编码保存
    d.Authorization = base64.StdEncoding.EncodeToString([]byte(prefix + ":" + account + ":" + newToken))
    op.MustSaveDriverStorage(d)  // 持久化到数据库
}
```

**迁移要点：**
- **这是令牌认证的核心模式**：解码 → 检查过期 → 刷新 → 重新编码保存
- `base.RestyClient` 可替换为项目自己的 HTTP 客户端
- `op.MustSaveDriverStorage(d)` 是持久化到数据库的框架调用，迁移时替换为对应存盘逻辑
- 启动一个定时器（cron）定期调用 `refreshToken()`（示例是每12小时）

### 3.3 定时刷新

```go
// 在 Init() 中启动
d.cron = cron.NewCron(time.Hour * 12)
d.cron.Do(func() {
    err := d.refreshToken()
    if err != nil { log.Errorf(...) }
})

// 在 Drop() 中停止
func (d *Yun139) Drop(ctx context.Context) error {
    if d.cron != nil { d.cron.Stop() }
    return nil
}
```

---

## 四、API 请求模式

### 4.1 通用请求（带签名）

所有请求均带签名（基于 `calSign` 函数），这是 139 云 API 的安全要求：

```go
func calSign(body, ts, randStr string) string {
    body = encodeURIComponent(body)
    strs := strings.Split(body, "")
    sort.Strings(strs)                                          // 1. 对字符排序
    body = strings.Join(strs, "")
    body = base64.StdEncoding.EncodeToString([]byte(body))     // 2. base64
    res := utils.GetMD5EncodeStr(body) + utils.GetMD5EncodeStr(ts+":"+randStr)
    res = strings.ToUpper(utils.GetMD5EncodeStr(res))          // 3. 双重 MD5
    return res
}
```

**迁移要点：**
- 如果目标项目 API 不需要签名，可跳过此函数
- 如果需要，核心思路：**对请求体做确定性变换 + 拼接时间戳/随机数 + 哈希**

### 4.2 请求头

```go
// 在 request() 中设置
headers := map[string]string{
    "Accept":              "application/json, text/plain, */*",
    "Authorization":       "Basic " + d.getAuthorization(),      // 令牌认证
    "CMS-DEVICE":          "default",
    "mcloud-channel":      "1000101",
    "mcloud-client":       "10701",
    "mcloud-sign":         fmt.Sprintf("%s,%s,%s", ts, randStr, sign),  // 签名
    "mcloud-version":      "7.14.0",
    "Origin":              "https://yun.139.com",
    "Referer":             "https://yun.139.com/w/",
    "x-DeviceInfo":        "||9|7.14.0|chrome|120.0.0.0|||windows 10||zh-CN|||",
    "x-m4c-caller":        "PC",
    "x-SvcType":           svcType,                              // "1"=个人云, "2"=家庭云
}
```

**家庭云与个人云的区别**主要在请求头中 `x-SvcType: "2"`（家庭云），以及使用不同的 API 主机地址。

### 4.3 请求方法层级

```
request()             ← 最底层：设置请求头 + 签名 + 执行请求 + 解析BaseResp
  ├── requestRoute()  ← 路由策略查询
  ├── post()          ← 个人云旧 API：yun.139.com + pathname
  ├── isboPost()      ← ISBO 接口
  ├── newRequest()    ← 新版请求（不同请求头）
  │   ├── personalPost() ← 新版个人云 API
  │   └── newPost()      ← 家庭云/共享群共用，自动路由到 GroupCloudHost
  └── andAlbumRequest()  ← 家庭云 AndAlbum 接口（加密请求）
```

**家庭云使用的主要方法：**

| 方法 | 用途 | 主机 |
|---|---|---|
| `newPost()` | 文件列表、链接、重命名、删除、旧上传 | `GroupCloudHost`（家庭云和共享群共用的主机） |
| `andAlbumRequest()` | 移动、复制、重命名文件夹（AndAlbum） | `FamilyCloudHost` + `/andAlbum/openApi` |
| `isboPost()` | 移动（ISBO 接口） | 固定 `group.yun.139.com/hcy/mutual/adapter` |

### 4.4 响应处理

```go
// 所有请求都先解析 BaseResp 检查 success
type BaseResp struct {
    Success bool   `json:"success"`
    Code    string `json:"code"`
    Message string `json:"message"`
}

// request() 处理流程
res, err := req.Execute(method, url)
if err != nil { return nil, err }
if !e.Success {
    // 特殊处理：CreateBatchOprTaskResp 的 ResultCode 为 "0" 也算成功
    if resp != nil {
        json.Unmarshal(res.Body(), resp)
        if createBatchOprTaskResp, ok := resp.(*CreateBatchOprTaskResp); ok {
            if createBatchOprTaskResp.Result.ResultCode == "0" { goto SUCCESS }
        }
    }
    return nil, errors.New(e.Message)
}
if resp != nil {
    json.Unmarshal(res.Body(), resp)
}
```

> 迁移时注意：不同 API 的"成功"判定标准可能不同（如 `resultCode == "0"` vs `success == true`）。

---

## 五、家庭云核心功能实现

### 5.1 初始化 -- 查询根路径

```go
func (d *Yun139) Init(ctx context.Context) error {
    // ---- 阶段1：认证 ----
    if d.ref == nil {  // 非引用模式
        if d.Authorization != "" {
            d.refreshToken()  // 刷新令牌
            // 查询路由策略，获取 FamilyCloudHost
            var resp QueryRoutePolicyResp
            d.requestRoute(body, &resp)
            for _, item := range resp.Data.RoutePolicyList {
                switch item.ModName {
                case "family": d.FamilyCloudHost = item.HttpsUrl
                }
            }
            // 启动定时刷新
            d.cron = cron.NewCron(time.Hour * 12)
            d.cron.Do(func() { d.refreshToken() })
        }
    }

    // ---- 阶段2：家庭云根路径（关键） ----
    case MetaFamily:
        root, err := d.getFamilyRootPath(d.CloudID)  // 查询家庭云根
        d.ProviderRoot = root                        // 存供后续使用
        d.RootFolderID = root                        // 持久化
        op.MustSaveDriverStorage(d)                   // 保存到DB
        d.familyGetFiles(d.RootFolderID)              // 验证可访问
}
```

**`getFamilyRootPath()` 实现：**

```go
func (d *Yun139) getFamilyRootPath(cloudID string) (string, error) {
    // 使用 v1.2 列表接口，取 1 条就够了（只需要 path 字段）
    body := base.Json{
        "catalogID":   "",
        "catalogType": 3,
        "cloudID":     cloudID,
        "cloudType":   1,
        "commonAccountInfo": base.Json{
            "account":     d.getAccount(),
            "accountType": 1,
        },
        "pageInfo": base.Json{"pageNum": 1, "pageSize": 1},
    }
    var resp base.Json
    d.post("/orchestration/familyCloud-rebuild/content/v1.2/queryContentList", body, &resp)
    dataObj := resp["data"].(map[string]interface{})
    path := strings.TrimPrefix(dataObj["path"].(string), "root:/")  // 去除 root:/ 前缀
    return path, nil
}
```

### 5.2 文件列表

```go
func (d *Yun139) familyGetFiles(catalogID string) ([]model.Obj, error) {
    pageNum := 1
    files := make([]model.Obj, 0)
    for {
        data := d.newJson(base.Json{                    // newJson() 自动注入 cloudID、account 等
            "catalogID":       catalogID,
            "contentSortType": 0,
            "pageInfo": base.Json{"pageNum": pageNum, "pageSize": 100},
            "sortDirection":   1,
        })
        if catalogID == d.ProviderRoot {
            data["catalogID"] = ""                       // 根目录时 catalogID 留空
        }

        var resp QueryContentListResp
        d.post("/orchestration/familyCloud-rebuild/content/v1.2/queryContentList", data, &resp)

        path := resp.Data.Path
        // 文件夹
        for _, catalog := range resp.Data.CloudCatalogList {
            f := model.Object{ID: catalog.CatalogID, Name: catalog.CatalogName,
                IsFolder: true, Modified: parseTime(catalog.LastUpdateTime), Path: path}
            files = append(files, &f)
        }
        // 文件（带缩略图）
        for _, content := range resp.Data.CloudContentList {
            f := model.ObjThumb{Object: model.Object{ID: content.ContentID, Name: content.ContentName,
                Size: content.ContentSize, Path: path},
                Thumbnail: model.Thumbnail{Thumbnail: content.ThumbnailURL}}
            files = append(files, &f)
        }
        if resp.Data.TotalCount == 0 { break }
        pageNum++
    }
    return files, nil
}
```

**通用模版（`newJson` 构造请求体）：**

```go
func (d *Yun139) newJson(data map[string]interface{}) base.Json {
    common := map[string]interface{}{
        "catalogType": 3,                           // 家庭云固定
        "cloudID":     d.CloudID,                   // 家庭云ID
        "cloudType":   1,
        "commonAccountInfo": base.Json{
            "account":     d.getAccount(),
            "accountType": 1,
        },
    }
    return utils.MergeMap(data, common)
}
```

### 5.3 List/Dispatch 模式

`List()` 和 `Link()` 都**根据 Type 路由到不同实现**：

```go
func (d *Yun139) List(ctx context.Context, dir model.Obj, args model.ListArgs) ([]model.Obj, error) {
    switch d.Addition.Type {
    case MetaFamily:   return d.familyGetFiles(dir.GetID())
    case MetaPersonal: return d.getFiles(dir.GetID())
    case MetaGroup:    return d.groupGetFiles(dir.GetID())
    // ...
    }
}
```

> **迁移建议**：如果目标项目只有一种模式，直接实现对应函数即可，不需要 switch。

### 5.4 获取下载链接

```go
func (d *Yun139) familyGetLink(contentId, path string) (string, error) {
    data := d.newJson(base.Json{
        "contentID": contentId,
        "path":      path,
    })
    res, err := d.post("/orchestration/familyCloud-rebuild/content/v1.0/getFileDownLoadURL", data, nil)
    return jsoniter.Get(res, "data", "downloadURL").ToString(), nil
}
```

### 5.5 写操作示例

**创建目录：**
```go
data := base.Json{
    "cloudID": d.CloudID,
    "commonAccountInfo": base.Json{
        "account": d.getAccount(), "accountType": 1,
    },
    "docLibName": dirName,
    "path":       d.dirPath(parentDir),
}
d.post("/orchestration/familyCloud-rebuild/cloudCatalog/v1.0/createCloudDoc", data, nil)
```

**删除：**
```go
data := base.Json{
    "catalogList": catalogInfoList,       // 文件夹ID列表
    "contentList": contentInfoList,       // 文件ID列表
    "commonAccountInfo": base.Json{
        "account": d.getAccount(), "accountType": 1,
    },
    "sourceCloudID":     d.CloudID,
    "sourceCatalogType": 1002,
    "taskType":          2,               // 2=删除
    "path":              obj.GetPath(),
}
d.post("/orchestration/familyCloud-rebuild/batchOprTask/v1.0/createBatchOprTask", data, nil)
```

**移动（ISBO 接口）：**
```go
body := base.Json{
    "catalogList":   catalogList,         // 源文件夹路径列表
    "contentList":   contentList,         // 源文件路径列表
    "destCatalogID": dstDir.GetID(),      // 目标目录ID
    "destGroupID":   d.CloudID,
    "destPath":      d.dirPath(dstDir),
    "srcGroupID":    d.CloudID,
    "taskType":      3,                   // 3=移动
    "accountInfo": base.Json{
        "accountName": d.getAccount(),
        "accountType": "1",
    },
}
d.isboPost("/isbo/openApi/createBatchOprTask", body, &resp)
```

### 5.6 上传文件 (Put)

139 家庭云支持**新/旧两条上传路径**：

**新上传路径（优先）：**
```
POST /dynamic/file/create         → 创建上传任务，获取上传地址
POST /dynamic/file/getUploadUrl   → 获取更多分片地址（超过100分片时）
PUT  <uploadUrl>                  → 上传分片内容
POST /dynamic/file/complete       → 完成上传
```

关键参数：
- 分片大小：默认为 100MB，大文件（>30GB）自动调整为 512MB
- 每次最多获取 100 个分片的上传地址
- 家庭云额外参数：`groupId`, `groupType: 1`, `catalogType: 3`, `seqNo`
- 支持 SHA256 内容哈希校验
- 冲突处理：检测到文件名冲突时自动删除旧文件 → 重命名新文件

---

## 六、Driver 接口清单

| 接口 | 方法 | 家庭云实现 | 说明 |
|---|---|---|---|
| `Meta` | `Config()` | ✅ 返回静态 config | 必需 |
| | `GetStorage()` | ✅ 内嵌 model.Storage | 必需 |
| | `SetStorage()` | ✅ 内嵌 model.Storage | 必需 |
| | `GetAddition()` | ✅ `return &d.Addition` | 必需 |
| | `Init(ctx)` | ✅ 认证+路由+根路径 | 必需 |
| | `Drop(ctx)` | ✅ 停止 cron | 必需 |
| `Reader` | `List(ctx, dir, args)` | ✅ `familyGetFiles()` | 必需 |
| | `Link(ctx, file, args)` | ✅ `familyGetLink()` | 必需 |
| `Mkdir` | `MakeDir()` | ✅ `createCloudDoc` | 可选 |
| `MoveResult` | `Move()` | ✅ `isboPost` | 可选 |
| `Rename` | `Rename()` | ✅ `modifyContentInfo` | 可选 |
| `Copy` | `Copy()` | ✅ `isboPost` | 可选 |
| `Remove` | `Remove()` | ✅ `createBatchOprTask` | 可选 |
| `Put` | `Put()` | ✅ 新/旧上传路径 | 可选 |
| `Other` | `Other()` | ❌ (仅 personal_new) | 可选 |
| `WithDetails` | `GetDetails()` | ✅ 需 `UserDomainID` | 可选 |
| `Reference` | `InitReference()` | ✅ 支持引用模式 | 可选 |

---

## 七、迁移步骤总结

### 步骤 1：建立基础文件结构

```
yourdriver/
├── meta.go       # Addition 结构体 + Config + init()
├── driver.go     # 主结构体 + 接口实现桩
├── util.go       # HTTP 请求工具、辅助函数
└── types.go      # JSON 结构定义
```

### 步骤 2：构建令牌认证

1. 定义 `Addition`，包含令牌字段（如 `Authorization`）
2. 在 `Init()` 中解码令牌，提取必要信息（账号、过期时间）
3. 实现令牌刷新函数（调用刷新 API → 重新编码保存）
4. 设置定时器定期刷新

### 步骤 3：实现 API 请求层

1. 确定目标项目 API 的认证方式（Basic Auth / Bearer Token / 自定义签名）
2. 建立统一的请求方法（设置请求头、签名、错误处理）
3. 确定 API 主机地址（从配置或接口获取）

### 步骤 4：实现核心接口

1. **`List()`** — 文件列表，关注分页、文件夹与文件的区分、缩略图
2. **`Link()`** — 获取下载链接
3. **`Init()`** — 认证 + 根路径查询 + 定时器

### 步骤 5：实现写操作（按需）

根据目标项目的能力，实现 `Put`（上传）、`Mkdir`、`Move`、`Rename`、`Copy`、`Remove`

### 步骤 6：注册驱动

```go
func init() {
    op.RegisterDriver(func() driver.Driver {
        return &YourDriver{}
    })
}
```

---

## 八、常见模式速查

| 场景 | 模式 |
|---|---|
| 带分页的列表 | `for { page++ ; if total==0 { break } }` |
| 家庭云与个人云共享请求方法 | `newPost()` 自动路由到不同主机 |
| 两层响应解析 | `BaseResp` 检错 + 具体类型取数据 |
| 路径处理 | `d.dirPath()` 拼接对象的 Path + ID |
| 请求体注入通用参数 | `newJson()` 自动注入 `cloudID`、`commonAccountInfo` |
| 令牌缓存与共享 | `ref` 字段 + `Reference` 接口 + `getAuthorization()` 委托 |
| 上传冲突 | 删除旧文件 → 重命名新文件名 |
