package wire

import "fmt"

// ---------------------------------------------------------------------------
// CHANGE_NOTIFY（MS-SMB2 §2.2.35 / §2.2.36）
//
// CHANGE_NOTIFY Request（§2.2.35），报文体**全部小端**：
//
//	 0 StructureSize(2) = 32
//	 2 Flags(2)                 SMB2_WATCH_TREE
//	 4 OutputBufferLength(4)
//	 8 FileId(16)
//	24 CompletionFilter(4)
//	28 Reserved(4)
//
// CHANGE_NOTIFY Response（§2.2.36）：
//
//	0 StructureSize(2) = 9
//	2 OutputBufferOffset(2)     相对 SMB2 头起点
//	4 OutputBufferLength(4)
//	8 Buffer                    FILE_NOTIFY_INFORMATION 链（MS-FSCC §2.7.1）
//
// 服务端行为要点（§3.3.5.19）：请求通常不能立刻应答，要先回
// STATUS_PENDING 的 interim response 并挂起，等真的有变更再发正式响应。
// 缓冲区放不下时回 STATUS_NOTIFY_ENUM_DIR(0x0000010C) 且**不带**任何条目，
// 让客户端自己重新枚举目录。
// ---------------------------------------------------------------------------

const (
	changeNotifyRequestStructureSize = 32
	changeNotifyRequestFixed         = 32

	changeNotifyResponseStructureSize = 9
	changeNotifyResponseFixed         = 8
)

// ChangeNotifyFlags 是 CHANGE_NOTIFY Request 的 Flags（MS-SMB2 §2.2.35）。
type ChangeNotifyFlags uint16

// SMB2_WATCH_TREE：监视整棵子树而不只是本目录。
const WatchTree ChangeNotifyFlags = 0x0001

// CompletionFilter 是关心哪些变更的位图（MS-SMB2 §2.2.35，取值同 MS-FSCC）。
type CompletionFilter uint32

// MS-SMB2 §2.2.35 — CompletionFilter
const (
	NotifyChangeFileName    CompletionFilter = 0x00000001 // FILE_NOTIFY_CHANGE_FILE_NAME
	NotifyChangeDirName     CompletionFilter = 0x00000002 // FILE_NOTIFY_CHANGE_DIR_NAME
	NotifyChangeAttributes  CompletionFilter = 0x00000004 // FILE_NOTIFY_CHANGE_ATTRIBUTES
	NotifyChangeSize        CompletionFilter = 0x00000008 // FILE_NOTIFY_CHANGE_SIZE
	NotifyChangeLastWrite   CompletionFilter = 0x00000010 // FILE_NOTIFY_CHANGE_LAST_WRITE
	NotifyChangeLastAccess  CompletionFilter = 0x00000020 // FILE_NOTIFY_CHANGE_LAST_ACCESS
	NotifyChangeCreation    CompletionFilter = 0x00000040 // FILE_NOTIFY_CHANGE_CREATION
	NotifyChangeEA          CompletionFilter = 0x00000080 // FILE_NOTIFY_CHANGE_EA
	NotifyChangeSecurity    CompletionFilter = 0x00000100 // FILE_NOTIFY_CHANGE_SECURITY
	NotifyChangeStreamName  CompletionFilter = 0x00000200 // FILE_NOTIFY_CHANGE_STREAM_NAME
	NotifyChangeStreamSize  CompletionFilter = 0x00000400 // FILE_NOTIFY_CHANGE_STREAM_SIZE
	NotifyChangeStreamWrite CompletionFilter = 0x00000800 // FILE_NOTIFY_CHANGE_STREAM_WRITE

	// CompletionFilterAll 是 §2.2.35 定义的全部有效位，用于校验：
	// 含有效位以外的比特应回 STATUS_INVALID_PARAMETER。
	CompletionFilterAll CompletionFilter = 0x00000FFF
)

// ChangeNotifyRequest 是 SMB2 CHANGE_NOTIFY Request（MS-SMB2 §2.2.35）。
type ChangeNotifyRequest struct {
	Flags              ChangeNotifyFlags
	OutputBufferLength uint32
	FileID             FileID
	CompletionFilter   CompletionFilter
}

// WatchTree 报告是否要求递归监视整棵子树。
func (r *ChangeNotifyRequest) WatchTree() bool { return r.Flags&WatchTree != 0 }

// ParseChangeNotifyRequest 解析 CHANGE_NOTIFY Request。b 是完整消息。
func ParseChangeNotifyRequest(b []byte) (*ChangeNotifyRequest, error) {
	body, err := msgBody(b, changeNotifyRequestFixed, "CHANGE_NOTIFY Request")
	if err != nil {
		return nil, err
	}
	if err := checkStructureSize(body, changeNotifyRequestStructureSize); err != nil {
		return nil, fmt.Errorf("CHANGE_NOTIFY Request: %w", err)
	}
	return &ChangeNotifyRequest{
		Flags:              ChangeNotifyFlags(le.Uint16(body[2:])),
		OutputBufferLength: le.Uint32(body[4:]),
		FileID:             parseFileID(body[8:]),
		CompletionFilter:   CompletionFilter(le.Uint32(body[24:])),
	}, nil
}

// Append 把 CHANGE_NOTIFY Request 报文体追加到 dst（供测试与 Go 客户端使用）。
//
// 注意：本请求的 StructureSize(32) 等于固定部分长度，**没有**可变部分，
// 因此不需要 1 字节占位（对比 QUERY_DIRECTORY 的 33/32）。
func (r *ChangeNotifyRequest) Append(dst []byte) []byte {
	dst, f := grow(dst, changeNotifyRequestFixed)
	le.PutUint16(f[0:], changeNotifyRequestStructureSize)
	le.PutUint16(f[2:], uint16(r.Flags))
	le.PutUint32(f[4:], r.OutputBufferLength)
	r.FileID.put(f[8:])
	le.PutUint32(f[24:], uint32(r.CompletionFilter))
	// f[28:32] Reserved
	return dst
}

// ChangeNotifyResponse 是 SMB2 CHANGE_NOTIFY Response（MS-SMB2 §2.2.36）。
// Buffer 是编码好的 FILE_NOTIFY_INFORMATION 链（用 NotifyWriter 生成）。
type ChangeNotifyResponse struct {
	Buffer []byte
}

// Append 把 CHANGE_NOTIFY Response 报文体追加到 dst。
func (r *ChangeNotifyResponse) Append(dst []byte) ([]byte, error) {
	dst, f := grow(dst, changeNotifyResponseFixed)
	le.PutUint16(f[0:], changeNotifyResponseStructureSize)
	n, err := u32(len(r.Buffer), "CHANGE_NOTIFY OutputBuffer")
	if err != nil {
		return nil, err
	}
	le.PutUint16(f[2:], HeaderSize+changeNotifyResponseFixed)
	le.PutUint32(f[4:], n)
	if n == 0 {
		// StructureSize(9) 比固定部分多 1，空缓冲时补 1 字节占位。
		dst, _ = grow(dst, 1)
		return dst, nil
	}
	return append(dst, r.Buffer...), nil
}

// ParseChangeNotifyResponse 解析 CHANGE_NOTIFY Response（供测试与客户端使用）。
func ParseChangeNotifyResponse(b []byte) (*ChangeNotifyResponse, error) {
	body, err := msgBody(b, changeNotifyResponseFixed, "CHANGE_NOTIFY Response")
	if err != nil {
		return nil, err
	}
	if err := checkStructureSize(body, changeNotifyResponseStructureSize); err != nil {
		return nil, fmt.Errorf("CHANGE_NOTIFY Response: %w", err)
	}
	buf, err := sliceAt(b, uint64(le.Uint16(body[2:])), uint64(le.Uint32(body[4:])))
	if err != nil {
		return nil, fmt.Errorf("CHANGE_NOTIFY Response OutputBuffer: %w", err)
	}
	return &ChangeNotifyResponse{Buffer: buf}, nil
}

// ---------------------------------------------------------------------------
// FILE_NOTIFY_INFORMATION（MS-FSCC §2.7.1）
//
//	 0 NextEntryOffset(4)   相对本条目起点；最后一条为 0
//	 4 Action(4)
//	 8 FileNameLength(4)    **字节数**，不是字符数
//	12 FileName(variable)   UTF-16LE，相对被监视目录的路径，无结尾 NUL
//
// 条目之间 **4 字节对齐**（注意不是目录枚举那样的 8 字节）。
// ---------------------------------------------------------------------------

// notifyInfoFixed 是 FILE_NOTIFY_INFORMATION 的固定部分长度。
const notifyInfoFixed = 12

// NotifyAction 是 FILE_NOTIFY_INFORMATION 的 Action（MS-FSCC §2.7.1）。
type NotifyAction uint32

// MS-FSCC §2.7.1 — Action
const (
	FileActionAdded              NotifyAction = 0x00000001 // FILE_ACTION_ADDED
	FileActionRemoved            NotifyAction = 0x00000002 // FILE_ACTION_REMOVED
	FileActionModified           NotifyAction = 0x00000003 // FILE_ACTION_MODIFIED
	FileActionRenamedOldName     NotifyAction = 0x00000004 // FILE_ACTION_RENAMED_OLD_NAME
	FileActionRenamedNewName     NotifyAction = 0x00000005 // FILE_ACTION_RENAMED_NEW_NAME
	FileActionAddedStream        NotifyAction = 0x00000006 // FILE_ACTION_ADDED_STREAM
	FileActionRemovedStream      NotifyAction = 0x00000007 // FILE_ACTION_REMOVED_STREAM
	FileActionModifiedStream     NotifyAction = 0x00000008 // FILE_ACTION_MODIFIED_STREAM
	FileActionRemovedByDelete    NotifyAction = 0x00000009 // FILE_ACTION_REMOVED_BY_DELETE
	FileActionIDNotTunnelled     NotifyAction = 0x0000000A // FILE_ACTION_ID_NOT_TUNNELLED
	FileActionTunnelledIDCollide NotifyAction = 0x0000000B // FILE_ACTION_TUNNELLED_ID_COLLISION
)

// NotifyEntry 是一条变更通知。Name 是相对被监视目录的路径，
// 用反斜杠分隔（例如 "sub\\a.txt"）。
type NotifyEntry struct {
	Action NotifyAction
	Name   string
}

// NotifyEntrySize 返回该条目编码后的字节数（未对齐）。
func NotifyEntrySize(e NotifyEntry) int { return notifyInfoFixed + UTF16LELen(e.Name) }

// align4 返回 v 向上取整到 4 的倍数。FILE_NOTIFY_INFORMATION 链按 4 字节对齐
// （MS-FSCC §2.7.1 NextEntryOffset）。
func align4(v int) int { return (v + 3) &^ 3 }

// NotifyWriter 生成 FILE_NOTIFY_INFORMATION 链，负责 **4 字节对齐**、
// NextEntryOffset 回填与 OutputBufferLength 上限控制。
//
// 放不下时调用方应丢弃整个缓冲区并回 STATUS_NOTIFY_ENUM_DIR（§3.3.5.19）。
type NotifyWriter struct {
	max       int
	buf       []byte
	lastStart int // 最后一条的起点，-1 表示还没有条目
	count     int
}

// NewNotifyWriter 创建写入器。max 是 OutputBufferLength 上限（字节）。
func NewNotifyWriter(max int) *NotifyWriter {
	return &NotifyWriter{max: max, lastStart: -1}
}

// Add 追加一条通知。返回 (false, nil) 表示放不下，缓冲区保持不变。
func (w *NotifyWriter) Add(e NotifyEntry) (bool, error) {
	nameLen, err := u32(UTF16LELen(e.Name), "FILE_NOTIFY_INFORMATION FileName")
	if err != nil {
		return false, err
	}
	start := len(w.buf)
	if w.lastStart >= 0 {
		start = align4(len(w.buf))
	}
	if start+notifyInfoFixed+int(nameLen) > w.max {
		return false, nil
	}
	if pad := start - len(w.buf); pad > 0 {
		w.buf, _ = grow(w.buf, pad)
	}
	if w.lastStart >= 0 {
		le.PutUint32(w.buf[w.lastStart:], uint32(start-w.lastStart))
	}
	w.buf, _ = grow(w.buf, notifyInfoFixed)
	f := w.buf[start:]
	// NextEntryOffset 留 0，由下一条回填。
	le.PutUint32(f[4:], uint32(e.Action))
	le.PutUint32(f[8:], nameLen)
	w.buf, _ = AppendUTF16LE(w.buf, e.Name)
	w.lastStart = start
	w.count++
	return true, nil
}

// Count 返回已写入的条目数。
func (w *NotifyWriter) Count() int { return w.count }

// Len 返回当前缓冲区字节数。
func (w *NotifyWriter) Len() int { return len(w.buf) }

// Bytes 返回通知链。最后一条的 NextEntryOffset 已是 0，尾部不补对齐填充。
func (w *NotifyWriter) Bytes() []byte { return w.buf }

// ParseNotifyEntries 解析 FILE_NOTIFY_INFORMATION 链（供测试与 Go 客户端使用）。
func ParseNotifyEntries(b []byte) ([]NotifyEntry, error) {
	if len(b) == 0 {
		return nil, nil
	}
	var out []NotifyEntry
	pos := uint64(0)
	for {
		f, err := sliceAt(b, pos, notifyInfoFixed)
		if err != nil {
			return nil, fmt.Errorf("FILE_NOTIFY_INFORMATION[%d]: %w", len(out), err)
		}
		next := uint64(le.Uint32(f[0:]))
		e := NotifyEntry{Action: NotifyAction(le.Uint32(f[4:]))}
		e.Name, err = DecodeUTF16LEAt(b, pos+notifyInfoFixed, uint64(le.Uint32(f[8:])))
		if err != nil {
			return nil, fmt.Errorf("FILE_NOTIFY_INFORMATION[%d] FileName: %w", len(out), err)
		}
		out = append(out, e)

		if next == 0 {
			return out, nil
		}
		if next < notifyInfoFixed || pos+next <= pos || pos+next > uint64(len(b)) {
			return nil, fmt.Errorf("%w: FILE_NOTIFY_INFORMATION[%d] NextEntryOffset=%d 非法",
				ErrMalformed, len(out)-1, next)
		}
		pos += next
	}
}
