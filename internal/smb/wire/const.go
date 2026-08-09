// Package wire 实现 SMB2/3 报文的**纯编解码**。
//
// 设计约束（AGENTS.md §5 P1）：
//
//   - 本包**无状态、无 IO**，只做 []byte ↔ struct，因此每个结构体都能用
//     固定字节向量做 golden test。
//   - SMB2 报文体一律**小端**（binary.LittleEndian）。Direct TCP 的 4 字节
//     长度前缀是**大端**，但那属于传输层（internal/server），不在本包。
//   - 解码路径的输入全部来自不可信网络：**必须先校验长度再切片**，
//     任何越界一律返回 error，**绝不 panic**。offset+length 一律用
//     uint64 运算防整数溢出。
//
// 编解码方法约定：
//
//	func (r *XxxRequest) Decode(b []byte) error            // b 不含 Direct TCP 头
//	func (r *XxxResponse) Encode(dst []byte) ([]byte, error) // 追加到 dst 并返回新切片
package wire

// Command 是 SMB2 命令码（MS-SMB2 §2.2.1.2 Command）。
type Command uint16

// MS-SMB2 §2.2.1.2 — SMB2 Header Command 取值。
const (
	CommandNegotiate      Command = 0x0000
	CommandSessionSetup   Command = 0x0001
	CommandLogoff         Command = 0x0002
	CommandTreeConnect    Command = 0x0003
	CommandTreeDisconnect Command = 0x0004
	CommandCreate         Command = 0x0005
	CommandClose          Command = 0x0006
	CommandFlush          Command = 0x0007
	CommandRead           Command = 0x0008
	CommandWrite          Command = 0x0009
	CommandLock           Command = 0x000A
	CommandIoctl          Command = 0x000B
	CommandCancel         Command = 0x000C
	CommandEcho           Command = 0x000D
	CommandQueryDirectory Command = 0x000E
	CommandChangeNotify   Command = 0x000F
	CommandQueryInfo      Command = 0x0010
	CommandSetInfo        Command = 0x0011
	CommandOplockBreak    Command = 0x0012
)

var commandNames = map[Command]string{
	CommandNegotiate:      "NEGOTIATE",
	CommandSessionSetup:   "SESSION_SETUP",
	CommandLogoff:         "LOGOFF",
	CommandTreeConnect:    "TREE_CONNECT",
	CommandTreeDisconnect: "TREE_DISCONNECT",
	CommandCreate:         "CREATE",
	CommandClose:          "CLOSE",
	CommandFlush:          "FLUSH",
	CommandRead:           "READ",
	CommandWrite:          "WRITE",
	CommandLock:           "LOCK",
	CommandIoctl:          "IOCTL",
	CommandCancel:         "CANCEL",
	CommandEcho:           "ECHO",
	CommandQueryDirectory: "QUERY_DIRECTORY",
	CommandChangeNotify:   "CHANGE_NOTIFY",
	CommandQueryInfo:      "QUERY_INFO",
	CommandSetInfo:        "SET_INFO",
	CommandOplockBreak:    "OPLOCK_BREAK",
}

// String 返回命令的符号名，未知命令返回 "COMMAND(0x00xx)"。
func (c Command) String() string {
	if n, ok := commandNames[c]; ok {
		return n
	}
	return "COMMAND(0x" + hex4(uint16(c)) + ")"
}

// Dialect 是 SMB 方言版本号（MS-SMB2 §2.2.3 / §2.2.4 DialectRevision）。
type Dialect uint16

// MS-SMB2 §2.2.3 SMB2 NEGOTIATE Request — Dialects
const (
	SMB202 Dialect = 0x0202 // SMB 2.0.2
	SMB210 Dialect = 0x0210 // SMB 2.1
	SMB300 Dialect = 0x0300 // SMB 3.0
	SMB302 Dialect = 0x0302 // SMB 3.0.2
	SMB311 Dialect = 0x0311 // SMB 3.1.1
	// SMB2Wildcard 仅用于回应 SMB1 多协议协商中的 "SMB 2.???"（MS-SMB2 §2.2.4）。
	SMB2Wildcard Dialect = 0x02FF
)

var dialectNames = map[Dialect]string{
	SMB202:       "SMB 2.0.2",
	SMB210:       "SMB 2.1",
	SMB300:       "SMB 3.0",
	SMB302:       "SMB 3.0.2",
	SMB311:       "SMB 3.1.1",
	SMB2Wildcard: "SMB 2.???",
}

// String 返回方言名。
func (d Dialect) String() string {
	if n, ok := dialectNames[d]; ok {
		return n
	}
	return "DIALECT(0x" + hex4(uint16(d)) + ")"
}

// IsSMB3 报告该方言是否属于 SMB 3.x 家族（决定签名算法与加密能力）。
func (d Dialect) IsSMB3() bool { return d >= SMB300 && d != SMB2Wildcard }

// Flags 是 SMB2 Header 的 Flags 字段（MS-SMB2 §2.2.1.2）。
type Flags uint32

// MS-SMB2 §2.2.1.2 — SMB2 Header Flags
const (
	FlagServerToRedir   Flags = 0x00000001 // SMB2_FLAGS_SERVER_TO_REDIR（响应置位）
	FlagAsyncCommand    Flags = 0x00000002 // SMB2_FLAGS_ASYNC_COMMAND
	FlagRelatedOps      Flags = 0x00000004 // SMB2_FLAGS_RELATED_OPERATIONS
	FlagSigned          Flags = 0x00000008 // SMB2_FLAGS_SIGNED
	FlagPriorityMask    Flags = 0x00000070 // SMB2_FLAGS_PRIORITY_MASK（3.1.1）
	FlagDFSOperations   Flags = 0x10000000 // SMB2_FLAGS_DFS_OPERATIONS
	FlagReplayOperation Flags = 0x20000000 // SMB2_FLAGS_REPLAY_OPERATION（3.x）
)

// Has 报告 f 是否包含 x 中的全部位。
func (f Flags) Has(x Flags) bool { return f&x == x }

// SecurityMode 是 NEGOTIATE / SESSION_SETUP 的 SecurityMode 位图。
// 注意：NEGOTIATE 里该字段是 2 字节，SESSION_SETUP 里是 1 字节，位值相同。
type SecurityMode uint16

// MS-SMB2 §2.2.3 / §2.2.5 — SecurityMode
const (
	NegotiateSigningEnabled  SecurityMode = 0x0001 // SMB2_NEGOTIATE_SIGNING_ENABLED
	NegotiateSigningRequired SecurityMode = 0x0002 // SMB2_NEGOTIATE_SIGNING_REQUIRED
)

// Capabilities 是 NEGOTIATE 的 Capabilities 位图。
type Capabilities uint32

// MS-SMB2 §2.2.3 — Capabilities
const (
	CapDFS               Capabilities = 0x00000001 // SMB2_GLOBAL_CAP_DFS
	CapLeasing           Capabilities = 0x00000002 // SMB2_GLOBAL_CAP_LEASING
	CapLargeMTU          Capabilities = 0x00000004 // SMB2_GLOBAL_CAP_LARGE_MTU
	CapMultiChannel      Capabilities = 0x00000008 // SMB2_GLOBAL_CAP_MULTI_CHANNEL
	CapPersistentHandles Capabilities = 0x00000010 // SMB2_GLOBAL_CAP_PERSISTENT_HANDLES
	CapDirectoryLeasing  Capabilities = 0x00000020 // SMB2_GLOBAL_CAP_DIRECTORY_LEASING
	CapEncryption        Capabilities = 0x00000040 // SMB2_GLOBAL_CAP_ENCRYPTION
)

// SessionFlags 是 SESSION_SETUP Response 的 SessionFlags（MS-SMB2 §2.2.6）。
type SessionFlags uint16

const (
	SessionFlagIsGuest     SessionFlags = 0x0001 // SMB2_SESSION_FLAG_IS_GUEST
	SessionFlagIsNull      SessionFlags = 0x0002 // SMB2_SESSION_FLAG_IS_NULL
	SessionFlagEncryptData SessionFlags = 0x0004 // SMB2_SESSION_FLAG_ENCRYPT_DATA
)

// SessionSetupFlags 是 SESSION_SETUP Request 的 Flags（MS-SMB2 §2.2.5）。
type SessionSetupFlags uint8

const (
	SessionSetupFlagBinding SessionSetupFlags = 0x01 // SMB2_SESSION_FLAG_BINDING
)

// ShareType 是 TREE_CONNECT Response 的 ShareType（MS-SMB2 §2.2.10）。
type ShareType uint8

const (
	ShareTypeDisk  ShareType = 0x01 // SMB2_SHARE_TYPE_DISK
	ShareTypePipe  ShareType = 0x02 // SMB2_SHARE_TYPE_PIPE
	ShareTypePrint ShareType = 0x03 // SMB2_SHARE_TYPE_PRINT
)

// ShareFlags 是 TREE_CONNECT Response 的 ShareFlags（MS-SMB2 §2.2.10）。
type ShareFlags uint32

const (
	ShareFlagManualCaching            ShareFlags = 0x00000000 // SMB2_SHAREFLAG_MANUAL_CACHING
	ShareFlagAutoCaching              ShareFlags = 0x00000010 // SMB2_SHAREFLAG_AUTO_CACHING
	ShareFlagVDOCaching               ShareFlags = 0x00000020 // SMB2_SHAREFLAG_VDO_CACHING
	ShareFlagNoCaching                ShareFlags = 0x00000030 // SMB2_SHAREFLAG_NO_CACHING
	ShareFlagDFS                      ShareFlags = 0x00000001 // SMB2_SHAREFLAG_DFS
	ShareFlagDFSRoot                  ShareFlags = 0x00000002 // SMB2_SHAREFLAG_DFS_ROOT
	ShareFlagRestrictExclusiveOpens   ShareFlags = 0x00000100 // SMB2_SHAREFLAG_RESTRICT_EXCLUSIVE_OPENS
	ShareFlagForceSharedDelete        ShareFlags = 0x00000200 // SMB2_SHAREFLAG_FORCE_SHARED_DELETE
	ShareFlagAllowNamespaceCaching    ShareFlags = 0x00000400 // SMB2_SHAREFLAG_ALLOW_NAMESPACE_CACHING
	ShareFlagAccessBasedDirectoryEnum ShareFlags = 0x00000800 // SMB2_SHAREFLAG_ACCESS_BASED_DIRECTORY_ENUM
	ShareFlagForceLevelIIOplock       ShareFlags = 0x00001000 // SMB2_SHAREFLAG_FORCE_LEVELII_OPLOCK
	ShareFlagEnableHashV1             ShareFlags = 0x00002000 // SMB2_SHAREFLAG_ENABLE_HASH_V1
	ShareFlagEnableHashV2             ShareFlags = 0x00004000 // SMB2_SHAREFLAG_ENABLE_HASH_V2
	ShareFlagEncryptData              ShareFlags = 0x00008000 // SMB2_SHAREFLAG_ENCRYPT_DATA
	ShareFlagIdentityRemoting         ShareFlags = 0x00040000 // SMB2_SHAREFLAG_IDENTITY_REMOTING
	ShareFlagCompressData             ShareFlags = 0x00100000 // SMB2_SHAREFLAG_COMPRESS_DATA
	ShareFlagIsolatedTransport        ShareFlags = 0x00200000 // SMB2_SHAREFLAG_ISOLATED_TRANSPORT
)

// ShareCapabilities 是 TREE_CONNECT Response 的 Capabilities（MS-SMB2 §2.2.10）。
type ShareCapabilities uint32

const (
	ShareCapDFS                    ShareCapabilities = 0x00000008 // SMB2_SHARE_CAP_DFS
	ShareCapContinuousAvailability ShareCapabilities = 0x00000010 // SMB2_SHARE_CAP_CONTINUOUS_AVAILABILITY
	ShareCapScaleout               ShareCapabilities = 0x00000020 // SMB2_SHARE_CAP_SCALEOUT
	ShareCapCluster                ShareCapabilities = 0x00000040 // SMB2_SHARE_CAP_CLUSTER
	ShareCapAsymmetric             ShareCapabilities = 0x00000080 // SMB2_SHARE_CAP_ASYMMETRIC
	ShareCapRedirectToOwner        ShareCapabilities = 0x00000100 // SMB2_SHARE_CAP_REDIRECT_TO_OWNER
)

// 常用的 MaximalAccess 取值（MS-SMB2 §2.2.10，值来自 MS-DTYP 访问掩码组合）。
const (
	// MaximalAccessReadWrite 是可读写共享的典型值（FILE_ALL_ACCESS）。
	MaximalAccessReadWrite uint32 = 0x001F01FF
	// MaximalAccessReadOnly 是只读共享的典型值（读+执行+同步）。
	MaximalAccessReadOnly uint32 = 0x00120089
)

// CreateDisposition 对应 MS-SMB2 §2.2.13 CreateDisposition。
type CreateDisposition uint32

const (
	FileSupersede   CreateDisposition = 0x00000000 // FILE_SUPERSEDE
	FileOpen        CreateDisposition = 0x00000001 // FILE_OPEN
	FileCreate      CreateDisposition = 0x00000002 // FILE_CREATE
	FileOpenIf      CreateDisposition = 0x00000003 // FILE_OPEN_IF
	FileOverwrite   CreateDisposition = 0x00000004 // FILE_OVERWRITE
	FileOverwriteIf CreateDisposition = 0x00000005 // FILE_OVERWRITE_IF
)

// CreateAction 是 CREATE Response 的 CreateAction（MS-SMB2 §2.2.14）。
type CreateAction uint32

const (
	FileSuperseded  CreateAction = 0x00000000 // FILE_SUPERSEDED
	FileOpened      CreateAction = 0x00000001 // FILE_OPENED
	FileCreated     CreateAction = 0x00000002 // FILE_CREATED
	FileOverwritten CreateAction = 0x00000003 // FILE_OVERWRITTEN
)

// CreateOptions 对应 MS-SMB2 §2.2.13 CreateOptions。
type CreateOptions uint32

const (
	FileDirectoryFile           CreateOptions = 0x00000001 // FILE_DIRECTORY_FILE
	FileWriteThrough            CreateOptions = 0x00000002 // FILE_WRITE_THROUGH
	FileSequentialOnly          CreateOptions = 0x00000004 // FILE_SEQUENTIAL_ONLY
	FileNoIntermediateBuffering CreateOptions = 0x00000008 // FILE_NO_INTERMEDIATE_BUFFERING
	FileSynchronousIOAlert      CreateOptions = 0x00000010 // FILE_SYNCHRONOUS_IO_ALERT
	FileSynchronousIONonAlert   CreateOptions = 0x00000020 // FILE_SYNCHRONOUS_IO_NONALERT
	FileNonDirectoryFile        CreateOptions = 0x00000040 // FILE_NON_DIRECTORY_FILE
	FileCompleteIfOplocked      CreateOptions = 0x00000100 // FILE_COMPLETE_IF_OPLOCKED
	FileNoEAKnowledge           CreateOptions = 0x00000200 // FILE_NO_EA_KNOWLEDGE
	FileRandomAccess            CreateOptions = 0x00000800 // FILE_RANDOM_ACCESS
	FileDeleteOnClose           CreateOptions = 0x00001000 // FILE_DELETE_ON_CLOSE
	FileOpenByFileID            CreateOptions = 0x00002000 // FILE_OPEN_BY_FILE_ID
	FileOpenForBackupIntent     CreateOptions = 0x00004000 // FILE_OPEN_FOR_BACKUP_INTENT
	FileNoCompression           CreateOptions = 0x00008000 // FILE_NO_COMPRESSION
	FileOpenRemoteInstance      CreateOptions = 0x00000400 // FILE_OPEN_REMOTE_INSTANCE
	FileOpenRequiringOplock     CreateOptions = 0x00010000 // FILE_OPEN_REQUIRING_OPLOCK
	FileDisallowExclusive       CreateOptions = 0x00020000 // FILE_DISALLOW_EXCLUSIVE
	FileReserveOpfilter         CreateOptions = 0x00100000 // FILE_RESERVE_OPFILTER
	FileOpenReparsePoint        CreateOptions = 0x00200000 // FILE_OPEN_REPARSE_POINT
	FileOpenNoRecall            CreateOptions = 0x00400000 // FILE_OPEN_NO_RECALL
	FileOpenForFreeSpaceQuery   CreateOptions = 0x00800000 // FILE_OPEN_FOR_FREE_SPACE_QUERY
)

// Access 是访问掩码（MS-SMB2 §2.2.13.1.1 File_Pipe_Printer_Access_Mask / MS-DTYP §2.4.3）。
type Access uint32

const (
	FileReadData        Access = 0x00000001 // FILE_READ_DATA / FILE_LIST_DIRECTORY
	FileWriteData       Access = 0x00000002 // FILE_WRITE_DATA / FILE_ADD_FILE
	FileAppendData      Access = 0x00000004 // FILE_APPEND_DATA / FILE_ADD_SUBDIRECTORY
	FileReadEA          Access = 0x00000008 // FILE_READ_EA
	FileWriteEA         Access = 0x00000010 // FILE_WRITE_EA
	FileExecute         Access = 0x00000020 // FILE_EXECUTE / FILE_TRAVERSE
	FileDeleteChild     Access = 0x00000040 // FILE_DELETE_CHILD
	FileReadAttributes  Access = 0x00000080 // FILE_READ_ATTRIBUTES
	FileWriteAttributes Access = 0x00000100 // FILE_WRITE_ATTRIBUTES
	Delete              Access = 0x00010000 // DELETE
	ReadControl         Access = 0x00020000 // READ_CONTROL
	WriteDAC            Access = 0x00040000 // WRITE_DAC
	WriteOwner          Access = 0x00080000 // WRITE_OWNER
	Synchronize         Access = 0x00100000 // SYNCHRONIZE（服务端忽略）
	AccessSystemSec     Access = 0x01000000 // ACCESS_SYSTEM_SECURITY
	MaximumAllowed      Access = 0x02000000 // MAXIMUM_ALLOWED
	GenericAll          Access = 0x10000000 // GENERIC_ALL
	GenericExecute      Access = 0x20000000 // GENERIC_EXECUTE
	GenericWrite        Access = 0x40000000 // GENERIC_WRITE
	GenericRead         Access = 0x80000000 // GENERIC_READ
)

// 展开 GENERIC_* 用的具体掩码（MS-DTYP §2.4.3 GenericMapping，文件对象）。
const (
	fileGenericRead    = FileReadData | FileReadAttributes | FileReadEA | ReadControl | Synchronize
	fileGenericWrite   = FileWriteData | FileAppendData | FileWriteAttributes | FileWriteEA | ReadControl | Synchronize
	fileGenericExecute = FileExecute | FileReadAttributes | ReadControl | Synchronize
	fileGenericAll     = 0x001F01FF
)

// Expand 展开 GENERIC_* 位为具体的文件访问位（MS-DTYP §2.4.3）。
// 调用方应在决定 O_RDONLY/O_WRONLY/O_RDWR 之前先调用它。
func (a Access) Expand() Access {
	out := a &^ (GenericAll | GenericExecute | GenericWrite | GenericRead)
	if a&GenericRead != 0 {
		out |= fileGenericRead
	}
	if a&GenericWrite != 0 {
		out |= fileGenericWrite
	}
	if a&GenericExecute != 0 {
		out |= fileGenericExecute
	}
	if a&GenericAll != 0 {
		out |= fileGenericAll
	}
	return out
}

// ShareAccess 是 CREATE 的 ShareAccess 位图（MS-SMB2 §2.2.13）。
type ShareAccess uint32

const (
	ShareRead   ShareAccess = 0x00000001 // FILE_SHARE_READ
	ShareWrite  ShareAccess = 0x00000002 // FILE_SHARE_WRITE
	ShareDelete ShareAccess = 0x00000004 // FILE_SHARE_DELETE
)

// ImpersonationLevel 是 CREATE 的 ImpersonationLevel（MS-SMB2 §2.2.13）。
type ImpersonationLevel uint32

const (
	ImpersonationAnonymous      ImpersonationLevel = 0x00000000
	ImpersonationIdentification ImpersonationLevel = 0x00000001
	ImpersonationImpersonation  ImpersonationLevel = 0x00000002
	ImpersonationDelegate       ImpersonationLevel = 0x00000003
)

// OplockLevel 是 CREATE 的 RequestedOplockLevel / 响应的 OplockLevel
// （MS-SMB2 §2.2.13）。
type OplockLevel uint8

const (
	OplockLevelNone      OplockLevel = 0x00 // SMB2_OPLOCK_LEVEL_NONE
	OplockLevelII        OplockLevel = 0x01 // SMB2_OPLOCK_LEVEL_II
	OplockLevelExclusive OplockLevel = 0x08 // SMB2_OPLOCK_LEVEL_EXCLUSIVE
	OplockLevelBatch     OplockLevel = 0x09 // SMB2_OPLOCK_LEVEL_BATCH
	OplockLevelLease     OplockLevel = 0xFF // SMB2_OPLOCK_LEVEL_LEASE
)

// FileAttributes 是 Windows 文件属性位图（MS-FSCC §2.6）。
type FileAttributes uint32

const (
	FileAttributeReadonly           FileAttributes = 0x00000001
	FileAttributeHidden             FileAttributes = 0x00000002
	FileAttributeSystem             FileAttributes = 0x00000004
	FileAttributeDirectory          FileAttributes = 0x00000010
	FileAttributeArchive            FileAttributes = 0x00000020
	FileAttributeNormal             FileAttributes = 0x00000080
	FileAttributeTemporary          FileAttributes = 0x00000100
	FileAttributeSparseFile         FileAttributes = 0x00000200
	FileAttributeReparsePoint       FileAttributes = 0x00000400
	FileAttributeCompressed         FileAttributes = 0x00000800
	FileAttributeOffline            FileAttributes = 0x00001000
	FileAttributeNotContentIndexed  FileAttributes = 0x00002000
	FileAttributeEncrypted          FileAttributes = 0x00004000
	FileAttributeIntegrityStream    FileAttributes = 0x00008000
	FileAttributeNoScrubData        FileAttributes = 0x00020000
	FileAttributeRecallOnOpen       FileAttributes = 0x00040000
	FileAttributePinned             FileAttributes = 0x00080000
	FileAttributeUnpinned           FileAttributes = 0x00100000
	FileAttributeRecallOnDataAccess FileAttributes = 0x00400000
)

// InfoType 是 QUERY_INFO / SET_INFO 的 InfoType（MS-SMB2 §2.2.37）。
type InfoType uint8

const (
	InfoTypeFile       InfoType = 0x01 // SMB2_0_INFO_FILE
	InfoTypeFileSystem InfoType = 0x02 // SMB2_0_INFO_FILESYSTEM
	InfoTypeSecurity   InfoType = 0x03 // SMB2_0_INFO_SECURITY
	InfoTypeQuota      InfoType = 0x04 // SMB2_0_INFO_QUOTA
)

// FileInfoClass 是 FileInformationClass（MS-FSCC §2.4）。
type FileInfoClass uint8

const (
	FileDirectoryInformation       FileInfoClass = 1
	FileFullDirectoryInformation   FileInfoClass = 2
	FileBothDirectoryInformation   FileInfoClass = 3
	FileBasicInformation           FileInfoClass = 4
	FileStandardInformation        FileInfoClass = 5
	FileInternalInformation        FileInfoClass = 6
	FileEaInformation              FileInfoClass = 7
	FileAccessInformation          FileInfoClass = 8
	FileNameInformation            FileInfoClass = 9
	FileRenameInformation          FileInfoClass = 10
	FileLinkInformation            FileInfoClass = 11
	FileNamesInformation           FileInfoClass = 12
	FileDispositionInformation     FileInfoClass = 13
	FilePositionInformation        FileInfoClass = 14
	FileFullEaInformation          FileInfoClass = 15
	FileModeInformation            FileInfoClass = 16
	FileAlignmentInformation       FileInfoClass = 17
	FileAllInformation             FileInfoClass = 18
	FileAllocationInformation      FileInfoClass = 19
	FileEndOfFileInformation       FileInfoClass = 20
	FileAlternateNameInformation   FileInfoClass = 21
	FileStreamInformation          FileInfoClass = 22
	FilePipeInformation            FileInfoClass = 23
	FileCompressionInformation     FileInfoClass = 28
	FileNetworkOpenInformation     FileInfoClass = 34
	FileAttributeTagInformation    FileInfoClass = 35
	FileIdBothDirectoryInformation FileInfoClass = 37 // 0x25
	FileIdFullDirectoryInformation FileInfoClass = 38 // 0x26
	FileValidDataLengthInformation FileInfoClass = 39
	FileShortNameInformation       FileInfoClass = 40
	FileIdInformation              FileInfoClass = 59
	FileNormalizedNameInformation  FileInfoClass = 48
)

// FsInfoClass 是 FileSystemInformationClass（MS-FSCC §2.5）。
type FsInfoClass uint8

const (
	FileFsVolumeInformation     FsInfoClass = 1
	FileFsLabelInformation      FsInfoClass = 2
	FileFsSizeInformation       FsInfoClass = 3
	FileFsDeviceInformation     FsInfoClass = 4
	FileFsAttributeInformation  FsInfoClass = 5
	FileFsControlInformation    FsInfoClass = 6
	FileFsFullSizeInformation   FsInfoClass = 7
	FileFsObjectIDInformation   FsInfoClass = 8
	FileFsSectorSizeInformation FsInfoClass = 11
)

// SecurityInfo 是 QUERY_INFO(SECURITY) 的 AdditionalInformation 位
// （MS-SMB2 §2.2.37）。
const (
	OwnerSecurityInformation uint32 = 0x00000001
	GroupSecurityInformation uint32 = 0x00000002
	DACLSecurityInformation  uint32 = 0x00000004
	SACLSecurityInformation  uint32 = 0x00000008
	LabelSecurityInformation uint32 = 0x00000010
)

// FileSystemAttributes 是 FileFsAttributeInformation 的属性位（MS-FSCC §2.5.1）。
const (
	FileCaseSensitiveSearch   uint32 = 0x00000001
	FileCasePreservedNames    uint32 = 0x00000002
	FileUnicodeOnDisk         uint32 = 0x00000004
	FilePersistentACLs        uint32 = 0x00000008
	FileFileCompression       uint32 = 0x00000010
	FileVolumeQuotas          uint32 = 0x00000020
	FileSupportsSparseFiles   uint32 = 0x00000040
	FileSupportsReparsePoints uint32 = 0x00000080
	FileVolumeIsCompressed    uint32 = 0x00008000
	FileSupportsObjectIDs     uint32 = 0x00010000
	FileSupportsEncryption    uint32 = 0x00020000
	FileNamedStreams          uint32 = 0x00040000
	FileReadOnlyVolume        uint32 = 0x00080000
	FileSequentialWriteOnce   uint32 = 0x00100000
	FileSupportsExtendedAttrs uint32 = 0x00800000
	FileSupportsBlockRefcount uint32 = 0x08000000
)

// 设备类型（MS-FSCC §2.5.10 FileFsDeviceInformation）。
const (
	FileDeviceDisk         uint32 = 0x00000007
	FileDeviceCDROM        uint32 = 0x00000002
	FileRemovableMedia     uint32 = 0x00000001
	FileReadOnlyDevice     uint32 = 0x00000002
	FileDeviceIsMounted    uint32 = 0x00000020
	FileVirtualVolume      uint32 = 0x00000040
	FileDeviceSecureOpen   uint32 = 0x00000100
	FileCharacteristicTS   uint32 = 0x00001000
	FileCharacteristicWebD uint32 = 0x00002000
)

// QueryDirectoryFlags 是 QUERY_DIRECTORY 的 Flags（MS-SMB2 §2.2.33）。
type QueryDirectoryFlags uint8

const (
	RestartScans       QueryDirectoryFlags = 0x01 // SMB2_RESTART_SCANS
	ReturnSingleEntry  QueryDirectoryFlags = 0x02 // SMB2_RETURN_SINGLE_ENTRY
	IndexSpecified     QueryDirectoryFlags = 0x04 // SMB2_INDEX_SPECIFIED
	ReopenQueryDirFlag QueryDirectoryFlags = 0x10 // SMB2_REOPEN
)

// Create Context 名（MS-SMB2 §2.2.13.2 请求侧 / §2.2.14.2 响应侧）。
//
// 规范把这些名字写成十六进制并注明「defined to be in network byte order」，
// 即 0x44483251 按**大端**展开就是 'D' 'H' '2' 'Q'。因此这里的 Go 字符串
// 字面量与线上字节序完全一致，无需再做字节序转换。
//
// ⚠️ 本注释曾经写着「DH2Q/DH2C 不在 §2.2.13.2 里」——**那是错的**，已核对
// 规范原文的 Name 取值表，DH2Q(0x44483251)/DH2C(0x44483243) 都在表内。
// 真正不在 MS 规范里的只有 AAPL 一个。
//
// 另有三个 16 字节 GUID 形式的名字（SMB2_CREATE_APP_INSTANCE_ID /
// _VERSION、SVHDX_OPEN_DEVICE_CONTEXT），本项目不实现，故不定义。
const (
	CreateContextExtA = "ExtA" // 0x45787441 SMB2_CREATE_EA_BUFFER
	CreateContextSecD = "SecD" // 0x53656344 SMB2_CREATE_SD_BUFFER
	CreateContextDHnQ = "DHnQ" // 0x44486E51 SMB2_CREATE_DURABLE_HANDLE_REQUEST / _RESPONSE
	CreateContextDHnC = "DHnC" // 0x44486E43 SMB2_CREATE_DURABLE_HANDLE_RECONNECT
	CreateContextAlSi = "AlSi" // 0x416C5369 SMB2_CREATE_ALLOCATION_SIZE
	CreateContextMxAc = "MxAc" // 0x4D784163 SMB2_CREATE_QUERY_MAXIMAL_ACCESS_REQUEST / _RESPONSE
	CreateContextTWrp = "TWrp" // 0x54577270 SMB2_CREATE_TIMEWARP_TOKEN
	CreateContextQFid = "QFid" // 0x51466964 SMB2_CREATE_QUERY_ON_DISK_ID
	// CreateContextRqLs 同时是 SMB2_CREATE_REQUEST_LEASE、_LEASE_V2 与
	// 响应侧 SMB2_CREATE_RESPONSE_LEASE、_LEASE_V2 —— 四者**共用同一个名字**，
	// v1/v2 靠 DataLength(32 vs 52) 区分（§2.2.13.2 / §2.2.14.2 原文如此）。
	CreateContextRqLs = "RqLs" // 0x52714C73
	CreateContextDH2Q = "DH2Q" // 0x44483251 SMB2_CREATE_DURABLE_HANDLE_REQUEST_V2 / _RESPONSE_V2
	CreateContextDH2C = "DH2C" // 0x44483243 SMB2_CREATE_DURABLE_HANDLE_RECONNECT_V2
	CreateContextAAPL = "AAPL" // Apple SMB2 扩展（非 MS 规范，见 Samba vfs_fruit）
)

// hex4 把 uint16 格式化为 4 位大写十六进制，避免 String() 依赖 fmt。
func hex4(v uint16) string {
	const digits = "0123456789ABCDEF"
	return string([]byte{
		digits[(v>>12)&0xF],
		digits[(v>>8)&0xF],
		digits[(v>>4)&0xF],
		digits[v&0xF],
	})
}
