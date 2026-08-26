package crypto

import (
	"bytes"
	"encoding/binary"
	"testing"
)

// TestGMACNISTVectors 用 NIST CAVP 官方测试向量（GCMVS，
// gcmEncryptExtIV128.rsp）钉住 GMAC 原语。
//
// 选用的用例全部满足 SP 800-38D 的 GMAC 口径：AES-128、96 位 IV、
// **明文为空**、数据全部作为 AAD、tag 128 位 —— 与 SMB2 签名的用法
// （MS-SMB2 §3.1.4.1 AES-GMAC）完全一致。
//
// 向量来源：NIST CAVP GCM Validation Suite（csrc.nist.gov,
// groups/STM/cavp/documents/mac/gcmtestvectors.zip），录入本文件前已用
// Go 标准库 cipher.NewGCM 交叉核对一致（标准库自身通过 CAVP 验证）。
func TestGMACNISTVectors(t *testing.T) {
	cases := []struct {
		name       string
		key, iv    string
		aad        string // GMAC 中即被认证的"消息"
		wantTag    string
		aadBitsNIST string // GCMVS 里该向量的 [AADlen]（位），留注释备查
	}{
		{
			name: "AAD=16字节", key: "77be63708971c4e240d1cb79e8d77feb",
			iv: "e0e00f19fed7ba0136a797f3", aad: "7a43ec1d9c0a5a78a0b16533a6213cab",
			wantTag: "209fcc8d3675ed938e9c7166709dd946", aadBitsNIST: "128",
		},
		{
			name: "AAD=20字节", key: "2fb45e5b8f993a2bfebc4b15b533e0b4",
			iv: "5b05755f984d2b90f94b8027", aad: "e85491b2202caf1d7dce03b97e09331c32473941",
			wantTag: "c75b7832b2a2d9bd827412b6ef5769db", aadBitsNIST: "160",
		},
		{
			name: "AAD=48字节", key: "99e3e8793e686e571d8285c564f75e2b",
			iv:     "c2dd0ab868da6aa8ad9c0d23",
			aad:    "b668e42d4e444ca8b23cfdd95a9fedd5178aa521144890b093733cf5cf22526c5917ee476541809ac6867a8c399309fc",
			wantTag: "3f4fba100eaf1f34b0baadaae9995d85", aadBitsNIST: "384",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got, err := GMAC(mustHex(t, tc.key), mustHex(t, tc.iv), mustHex(t, tc.aad))
			if err != nil {
				t.Fatalf("GMAC 失败: %v", err)
			}
			if !bytes.Equal(got, mustHex(t, tc.wantTag)) {
				t.Fatalf("tag 不匹配\n got=%x\nwant=%s", got, tc.wantTag)
			}
		})
	}
}

// gmacTestMessage 构造一条最小可签名的 SMB2 消息，字段可注入。
func gmacTestMessage(msgID uint64, flags uint32, cmd uint16) []byte {
	msg := make([]byte, HeaderSize+16)
	copy(msg[0:], []byte{0xFE, 'S', 'M', 'B'}) // ProtocolId
	binary.LittleEndian.PutUint16(msg[4:], 64) // StructureSize
	binary.LittleEndian.PutUint16(msg[offCommand:], cmd)
	binary.LittleEndian.PutUint32(msg[offFlags:], flags)
	binary.LittleEndian.PutUint64(msg[offMessageID:], msgID)
	return msg
}

// TestGMACNonceConstruction 钉死 MS-SMB2 §3.1.4.1 的 nonce 语法：
//
//	12 字节 = 前 8 字节 MessageId（小端原样）
//	        + 第 9 字节 bit0=服务端发出(SMB2_FLAGS_SERVER_TO_REDIR)、bit1=SMB2 CANCEL
//	        + 其余 3 字节为 0
func TestGMACNonceConstruction(t *testing.T) {
	const testMsgID = 0x0102030405060708

	t.Run("客户端请求", func(t *testing.T) {
		msg := gmacTestMessage(testMsgID, 0, 0x0006) // 无 S2R，非 CANCEL
		n := gmacNonce(msg)
		if got := binary.LittleEndian.Uint64(n[:8]); got != testMsgID {
			t.Fatalf("nonce 前 8 字节应为 MessageId，got %#x want %#x", got, testMsgID)
		}
		if n[8] != 0x00 {
			t.Fatalf("客户端请求的标志字节应为 0x00，got %#x", n[8])
		}
	})
	t.Run("服务端响应置S2R位", func(t *testing.T) {
		msg := gmacTestMessage(testMsgID, flagsServerToRedir, 0x0006)
		if n := gmacNonce(msg); n[8] != 0x01 {
			t.Fatalf("S2R 应置 bit0 (0x01)，got %#x", n[8])
		}
	})
	t.Run("CANCEL请求", func(t *testing.T) {
		msg := gmacTestMessage(testMsgID, 0, commandCancel) // SMB2_CANCEL = 0x000C
		if n := gmacNonce(msg); n[8] != 0x02 {
			t.Fatalf("CANCEL 应置 bit1 (0x02)，got %#x", n[8])
		}
	})
	t.Run("服务端CANCEL两位同置", func(t *testing.T) {
		msg := gmacTestMessage(testMsgID, flagsServerToRedir, commandCancel)
		if n := gmacNonce(msg); n[8] != 0x03 {
			t.Fatalf("S2R+CANCEL 应为 0x03，got %#x", n[8])
		}
	})
	t.Run("尾部三字节恒为零", func(t *testing.T) {
		msg := gmacTestMessage(testMsgID, flagsServerToRedir|flagsSigned, commandCancel)
		n := gmacNonce(msg)
		for i := 9; i < 12; i++ {
			if n[i] != 0 {
				t.Fatalf("nonce[%d] 应为 0，got %#x", i, n[i])
			}
		}
	})
}

// TestGMACSignVerifyRoundtrip：aes-gmac 配置下的 sign/verify 自洽，
// 且签名对 nonce 输入（MessageId / 方向 / CANCEL 位）敏感 ——
// 换任何一个输入都必须得到不同 tag 并验签失败，防止"nonce 恒定"
// 这类静默退化。
func TestGMACSignVerifyRoundtrip(t *testing.T) {
	key := []byte("0123456789abcdef") // 16 字节，AES-128

	mkSigned := func(msgID uint64, flags uint32, cmd uint16) []byte {
		msg := gmacTestMessage(msgID, flags, cmd)
		if err := SignWith(SigningAESGMAC, key, msg); err != nil {
			t.Fatalf("签名失败: %v", err)
		}
		return msg
	}

	base := mkSigned(0x1122334455667788, flagsServerToRedir, 0x0006)
	if err := VerifyWith(SigningAESGMAC, key, base); err != nil {
		t.Fatalf("roundtrip 验签失败: %v", err)
	}

	t.Run("不同MessageId签名不同", func(t *testing.T) {
		other := mkSigned(0x1122334455667789, flagsServerToRedir, 0x0006)
		if bytes.Equal(base[offSignature:offSignature+SignatureSize],
			other[offSignature:offSignature+SignatureSize]) {
			t.Fatal("不同 MessageId 得到相同签名 —— nonce 未参与计算")
		}
	})
	t.Run("不同方向位签名不同", func(t *testing.T) {
		client := gmacTestMessage(0x1122334455667788, 0, 0x0006)
		if err := SignWith(SigningAESGMAC, key, client); err != nil {
			t.Fatalf("签名失败: %v", err)
		}
		if bytes.Equal(base[offSignature:offSignature+SignatureSize],
			client[offSignature:offSignature+SignatureSize]) {
			t.Fatal("S2R 位不同的两条消息得到相同签名")
		}
	})
	t.Run("篡改报文体验签失败", func(t *testing.T) {
		tampered := append([]byte(nil), base...)
		tampered[HeaderSize] ^= 0xFF
		if err := VerifyWith(SigningAESGMAC, key, tampered); err != ErrBadSignature {
			t.Fatalf("期望 ErrBadSignature，实际 %v", err)
		}
	})
	t.Run("换密钥验签失败", func(t *testing.T) {
		if err := VerifyWith(SigningAESGMAC, []byte("fedcba9876543210"), base); err != ErrBadSignature {
			t.Fatalf("期望 ErrBadSignature，实际 %v", err)
		}
	})
}
