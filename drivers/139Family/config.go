package family139

// Addition 是移动家庭云账号配置。Authorization 实际落到认证状态表。
type Addition struct {
	Authorization string `json:"authorization" label:"Authorization 令牌" type:"password" form:"required,full"`
	CloudID       string `json:"cloud_id"      label:"家庭云ID"       form:"required,pair=opts"`
	RootFolderID  string `json:"root_folder_id" label:"根目录ID"      default:"" form:"pair=opts"`
	DownloadMode  string `json:"download_mode" label:"下载模式" type:"select" options:"redirect:302重定向,proxy:本机代理" default:"redirect" form:"pair=opts2"`
	UploadPartSize int64 `json:"custom_upload_part_size" label:"上传分片大小(MB)" type:"number" default:"100" form:"pair=opts2"`
}
