package mdns

import "fmt"

// 本文件里的 TXT 字段取值**不是编造的**（AGENTS.md §9）。
//
// 主要来源：Samba + avahi 的 Time Machine 配方，即社区里被反复验证过的
// /etc/avahi/services/samba.service：
//
//	<service><type>_smb._tcp</type><port>445</port></service>
//	<service><type>_device-info._tcp</type><port>0</port>
//	  <txt-record>model=TimeCapsule8,119</txt-record></service>
//	<service><type>_adisk._tcp</type>
//	  <txt-record>dk0=adVN=timemachine,adVF=0x82</txt-record>
//	  <txt-record>sys=waMa=0,adVF=0x100</txt-record></service>
//
// 这套写法在 macOS Ventura 上能让共享自动出现在"时间机器"的磁盘列表里。
// 各个比特位的确切语义没有任何公开规范给出定义，凡是拿不准的都标了
// `TODO: 待真实抓包验证`，不做臆测。

// Apple 生态相关的 DNS-SD 服务类型。
const (
	// serviceTypeSMB 是 SMB 文件共享（IANA 注册的 DNS-SD 服务类型）。
	serviceTypeSMB = "_smb._tcp"

	// serviceTypeDeviceInfo 是 Apple 的设备信息服务。
	// Finder 侧边栏用它的 TXT model= 决定显示哪个图标。
	serviceTypeDeviceInfo = "_device-info._tcp"

	// serviceTypeADisk 是 Apple 的 Time Machine 磁盘宣告服务。
	// "adisk" = Apple Disk。
	serviceTypeADisk = "_adisk._tcp"
)

const (
	// deviceInfoPort：_device-info._tcp 没有真实端口，信息全在 TXT 里。
	// avahi 配方里写的就是 <port>0</port>。
	deviceInfoPort uint16 = 0

	// adiskPort：_adisk._tcp 同样没有真实端口。
	// 上面的 avahi 配方直接省略了 <port>（等价于 0）；另有一些配方写 9
	// （discard 端口）。两种写法 macOS 都能识别，这里取 0。
	adiskPort uint16 = 0
)

// adiskSysTXT 是 _adisk._tcp 的 "sys=" 条目，描述整台设备而不是单个卷。
//
//	waMa=0      TODO: 待真实抓包验证 —— 疑似 "wireless MAC address"，
//	            真实 Time Capsule 会填自己的 MAC；配方里一律填 0，
//	            macOS 接受。没有公开规范，先照抄。
//	adVF=0x100  TODO: 待真实抓包验证 —— 设备级的 advertised volume flags。
//	            按位含义未见公开定义。
const adiskSysTXT = "sys=waMa=0,adVF=0x100"

// adiskVolumeFlags 是每个 Time Machine 卷的 adVF 取值。
//
// 0x82 是 Samba / netatalk 两套配方里通用的值。
// TODO: 待真实抓包验证 —— 一般认为是 0x80（支持 Time Machine 备份）
// 与 0x02（卷可见/可用）的按位或，但没有权威文档，不在代码里依赖这个拆分。
const adiskVolumeFlags = "0x82"

// smbServiceDef 构造 _smb._tcp 服务定义。
//
// SMB 的 DNS-SD 记录不需要 TXT（RFC 6763 §6.1 要求至少发一个空的
// character-string，TXT{} 的编码已经处理了这一点）。
func smbServiceDef(port uint16) serviceDef {
	return serviceDef{Type: serviceTypeSMB, Port: port}
}

// deviceInfoServiceDef 构造 _device-info._tcp 服务定义。
//
// model 的常见取值：
//   - "MacSamba"          Samba 自己的默认值（fruit:model），显示成普通服务器图标
//   - "Xserve"            服务器机架图标
//   - "TimeCapsule8,119"  Time Capsule 图标，Time Machine 配方里常用
//   - "RackMac"           机架式 Mac
func deviceInfoServiceDef(model string) serviceDef {
	return serviceDef{
		Type: serviceTypeDeviceInfo,
		Port: deviceInfoPort,
		TXT:  []string{"model=" + model},
	}
}

// adiskServiceDef 构造 _adisk._tcp 服务定义，宣告可用于 Time Machine 的卷。
//
// volumes 是共享名列表，按顺序编号成 dk0、dk1 …（RFC 6763 §6.3 的键值对）。
// 共享名里的 "," 与 "=" 已被 config 层禁止，不会破坏 dkN 的子格式。
//
// volumes 为空时返回 ok=false：没有可备份的卷就不该宣告这个服务。
func adiskServiceDef(volumes []string) (serviceDef, bool) {
	if len(volumes) == 0 {
		return serviceDef{}, false
	}
	txt := make([]string, 0, len(volumes)+1)
	for i, v := range volumes {
		txt = append(txt, fmt.Sprintf("dk%d=adVN=%s,adVF=%s", i, v, adiskVolumeFlags))
	}
	// 配方里 sys= 排在各 dkN 之后。TXT 条目顺序在 DNS-SD 里无语义
	// （RFC 6763 §6.4），这里只是与配方保持一致，便于抓包比对。
	txt = append(txt, adiskSysTXT)

	return serviceDef{Type: serviceTypeADisk, Port: adiskPort, TXT: txt}, true
}
