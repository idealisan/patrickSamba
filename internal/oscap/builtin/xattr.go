package builtin

// xattr.go —— CapXattr 的 builtin 实现：属性直接存进旁路 KV。
//
// 与 native 的一个重要差别：这里**没有平台命名空间前缀**（Linux 的 `user.`）。
// 名字进来什么样就存什么样、列出来还是什么样，所以 ports.go 反复强调的
// 「ListXattr 报出来的名字必须能拿去 GetXattr 取到」在本实现里是构造性成立的。

import (
	"github.com/idealisan/patrickSamba/internal/oscap"
)

// GetXattr 实现 oscap.Xattr。
func (a *adapter) GetXattr(ref oscap.Ref, name string) ([]byte, error) {
	if name == "" {
		return nil, oscap.ErrInvalidArg
	}
	v, ok, err := a.st.get(bucketXattr, a.st.subKey(ref.Path, name))
	if err != nil {
		return nil, err
	}
	if !ok {
		return nil, oscap.ErrNotFound
	}
	// v 来自 store.get，零长度时也是**非 nil** 的空切片：
	//「存在但为空」与「不存在」必须能被调用方区分开。
	return v, nil
}

// SetXattr 实现 oscap.Xattr。
func (a *adapter) SetXattr(ref oscap.Ref, name string, value []byte) error {
	if name == "" {
		return oscap.ErrInvalidArg
	}
	return a.st.put(bucketXattr, a.st.subKey(ref.Path, name), value)
}

// RemoveXattr 实现 oscap.Xattr。
func (a *adapter) RemoveXattr(ref oscap.Ref, name string) error {
	if name == "" {
		return oscap.ErrInvalidArg
	}
	existed, err := a.st.del(bucketXattr, a.st.subKey(ref.Path, name))
	if err != nil {
		return err
	}
	if !existed {
		return oscap.ErrNotFound
	}
	return nil
}

// ListXattr 实现 oscap.Xattr。
//
// 一个属性都没有时返回 (nil, nil) —— 空结果是合法答案，不是错误。
func (a *adapter) ListXattr(ref oscap.Ref) ([]string, error) {
	pairs, err := a.st.scanPrefix(bucketXattr, a.st.subPrefix(ref.Path))
	if err != nil {
		return nil, err
	}
	if len(pairs) == 0 {
		return nil, nil
	}
	names := make([]string, 0, len(pairs))
	for _, p := range pairs {
		names = append(names, p.Key)
	}
	return names, nil
}
