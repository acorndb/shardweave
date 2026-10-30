package mysql

// Packet headers.
const (
	OKHeader  = 0x00
	ErrHeader = 0xff
	EOFHeader = 0xfe
)

// Command codes (COM_*).
const (
	ComQuit             = 0x01
	ComInitDB           = 0x02
	ComQuery            = 0x03
	ComFieldList        = 0x04
	ComPing             = 0x0e
	ComStmtPrepare      = 0x16
	ComStmtExecute      = 0x17
	ComStmtSendLongData = 0x18
	ComStmtClose        = 0x19
	ComStmtReset        = 0x1a
	ComResetConnection  = 0x1f
)

// Client capability flags.
const (
	ClientLongPassword               uint32 = 1 << 0
	ClientFoundRows                  uint32 = 1 << 1
	ClientLongFlag                   uint32 = 1 << 2
	ClientConnectWithDB              uint32 = 1 << 3
	ClientNoSchema                   uint32 = 1 << 4
	ClientCompress                   uint32 = 1 << 5
	ClientODBC                       uint32 = 1 << 6
	ClientLocalFiles                 uint32 = 1 << 7
	ClientIgnoreSpace                uint32 = 1 << 8
	ClientProtocol41                 uint32 = 1 << 9
	ClientInteractive                uint32 = 1 << 10
	ClientSSL                        uint32 = 1 << 11
	ClientIgnoreSigpipe              uint32 = 1 << 12
	ClientTransactions               uint32 = 1 << 13
	ClientReserved                   uint32 = 1 << 14
	ClientSecureConnection           uint32 = 1 << 15
	ClientMultiStatements            uint32 = 1 << 16
	ClientMultiResults               uint32 = 1 << 17
	ClientPSMultiResults             uint32 = 1 << 18
	ClientPluginAuth                 uint32 = 1 << 19
	ClientConnectAttrs               uint32 = 1 << 20
	ClientPluginAuthLenEncClientData uint32 = 1 << 21
	ClientCanHandleExpiredPasswords  uint32 = 1 << 22
	ClientSessionTrack               uint32 = 1 << 23
	ClientDeprecateEOF               uint32 = 1 << 24
)

// Server status flags (OK / EOF packets).
const (
	ServerStatusInTrans         uint16 = 0x0001
	ServerStatusAutocommit      uint16 = 0x0002
	ServerMoreResultsExists     uint16 = 0x0008
	ServerStatusNoGoodIndexUsed uint16 = 0x0010
	ServerStatusNoIndexUsed     uint16 = 0x0020
	ServerStatusCursorExists    uint16 = 0x0040
	ServerStatusLastRowSend     uint16 = 0x0080
	ServerStatusDBDropped       uint16 = 0x0100
	ServerStatusMetadataChanged uint16 = 0x0400
)
