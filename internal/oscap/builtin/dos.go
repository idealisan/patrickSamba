package builtin

// dos.go —— CapDOSAttributes 的 builtin 实现。
//
// 边界（ports.go 已划好，这里只是兑现）：本能力**只回答**「有没有人显式设置过
// DOS 属性位、设的是什么」。由文件系统客观事实推出来的位（DIRECTORY / SPARSE /
// REPARSE_POINT）以及 POSIX 约定合成的位（点开头 → HIDDEN、属主无写权限 →
// READONLY）由 vfs 层合成。
//
// 所以「从来没被设置过」必须如实报 ErrNotFound，而不是返回 0 —— 0 是一个
// 合法的属性值（客户端可以显式把所有位清掉），把两者混为一谈会让 vfs 无法
// 区分「用户要求清空」与「没人管过，你自己合成吧」。

import (
	"encoding/binary"
	"fmt"

	"github.com/idealisan/patrickSamba/internal/oscap"
)

// dosRecordLen 是一条 DOS 属性记录的字节数：uint32，显式小端。
const dosRecordLen = 4

// DOSAttributes 实现 oscap.DOSAttributes。
func (a *adapter) DOSAttributes(ref oscap.Ref) (uint32, error) {
	v, ok, err := a.st.get(bucketDOS, a.st.objKey(ref.Path))
	if err != nil {
		return 0, err
	}
	if !ok {
		return 0, oscap.ErrNotFound
	}
	if len(v) != dosRecordLen {
		return 0, fmt.Errorf("%w: dosattr 记录长度 %d", errCorrupt, len(v))
	}
	return binary.LittleEndian.Uint32(v), nil
}

// SetDOSAttributes 实现 oscap.DOSAttributes。
func (a *adapter) SetDOSAttributes(ref oscap.Ref, attrs uint32) error {
	var rec [dosRecordLen]byte
	binary.LittleEndian.PutUint32(rec[:], attrs)
	return a.st.put(bucketDOS, a.st.objKey(ref.Path), rec[:])
}
