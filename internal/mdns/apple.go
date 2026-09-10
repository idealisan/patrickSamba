package mdns

import (
	"fmt"
	"strings"
)

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
//	waMa=0      疑似 "wireless MAC address"：真实 Time Capsule 会填自己的
//	            MAC，社区配方一律填 0，macOS 接受。这个缩写没有任何公开
//	            文档定义过，是照抄，不是推导出来的。
//	adVF=0x100  设备级的 advertised volume flags。按位含义未见公开定义。
//
// 这两个取值的**来源与可信度**（与 adiskVolumeFlags 相同，见那里的说明）：
// avahi / Samba 社区配方里的经验值，不是规范给的；v0.1.0 的 Time Machine
// 验收里它们被真机跑通过（docs/timemachine-v0.1.0.md），残余风险与现状
// 记录在 docs/timemachine-status.md「待验证项」一节。
//
// 这里刻意**不写** "TODO: 待真实抓包验证" 那种措辞：抓包只能证明「macOS
// 接受了」，证明不了「每一位是什么含义」——而后者根本没有公开规范可查。
// 把不可得的东西写成待办，只会让人以为再抓一次包就能确定。
const adiskSysTXT = "sys=waMa=0,adVF=0x100"

// adiskVolumeFlags 是每个 Time Machine 卷的 adVF 取值。
//
// 0x82 是 Samba / netatalk / avahi 三套配方里通用的值。社区里流传的拆分是
// 0x80（支持 Time Machine 备份）| 0x02（卷可见/可用），但**没有任何公开
// 规范给出这个按位定义**，因此：
//
//   - 代码不依赖这个拆分：这里就是一个整体字符串常量，没有按位运算，
//     也没有任何分支以 0x80 / 0x02 为条件；
//   - 注释也不把它写成事实，只记为「流传的拆分」。
//
// 可验证的部分是「用这个值 macOS 认」：v0.1.0 的 TM 验收里，带这条 TXT 的
// 共享能被 macOS 发现并在时间机器偏好设置里选为备份目的地
// （docs/timemachine-v0.1.0.md）。位级语义仍属未知，残余风险记录见
// docs/timemachine-status.md「待验证项」。
const adiskVolumeFlags = "0x82"

// adiskEscapeName 转义 dkN 条目里卷名的分隔符。
//
// 条目形如 `dk0=adVN=<name>,adVF=0x82`：逗号是字段分隔符、反斜杠是转义符，
// 所以卷名自身含 `,` 或 `\` 时必须转义，否则条目会被截断或被误读成转义序列。
//
// config 层已经把这两个字符连同其它 Windows 非法字符一起禁掉了
// （internal/config/validate.go 的 shareNameInvalidChars），正常情况下到不了
// 这里；仍然转义是**防御性**的：这条 TXT 的正确性不该依赖调用方先校验过。
func adiskEscapeName(name string) string {
	if !strings.ContainsAny(name, `,\`) {
		return name
	}
	var b strings.Builder
	b.Grow(len(name) + 2)
	for _, r := range name {
		if r == ',' || r == '\\' {
			b.WriteByte('\\')
		}
		b.WriteRune(r)
	}
	return b.String()
}

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
// 卷名里的 `,` 与 `\` 由 adiskEscapeName 转义；`=` 由 config 层禁止
// （shareNameInvalidChars），不会破坏 dkN= 的子格式。
//
// volumes 为空时返回 ok=false：没有可备份的卷就不该宣告这个服务。
func adiskServiceDef(volumes []string) (serviceDef, bool) {
	if len(volumes) == 0 {
		return serviceDef{}, false
	}
	txt := make([]string, 0, len(volumes)+1)
	for i, v := range volumes {
		txt = append(txt, fmt.Sprintf("dk%d=adVN=%s,adVF=%s", i, adiskEscapeName(v), adiskVolumeFlags))
	}
	// 配方里 sys= 排在各 dkN 之后。TXT 条目顺序在 DNS-SD 里无语义
	// （RFC 6763 §6.4），这里只是与配方保持一致，便于抓包比对。
	txt = append(txt, adiskSysTXT)

	return serviceDef{Type: serviceTypeADisk, Port: adiskPort, TXT: txt}, true
}
