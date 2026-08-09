// 独立 module：验收测试用的 Go SMB 客户端。
// 故意与主模块分离，避免把客户端库依赖污染 stupidsamba 的 go.mod。
module gosmb2client

go 1.23

require github.com/hirochachacha/go-smb2 v1.1.0

require (
	github.com/geoffgarside/ber v1.1.0 // indirect
	golang.org/x/crypto v0.0.0-20200728195943-123391ffb6de // indirect
)
