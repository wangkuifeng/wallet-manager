package main

import (
	"bytes"
	"context"
	"encoding/hex"
	"encoding/json"
	"log"
	"math/big"
	"net/http"
	"time"

	"github.com/ethereum/go-ethereum/common"
	"github.com/ethereum/go-ethereum/core/types"
	"github.com/ethereum/go-ethereum/ethclient"
	"gorm.io/driver/mysql"
	"gorm.io/gorm"
)

// --- 配置常量 ---
const (
	RPC_URL         = "https://sepolia.infura.io/v3/YOUR_API_KEY" // 建议用 Sepolia 测试网
	DSN             = "root:123456@tcp(127.0.0.1:3306)/business_wallet?charset=utf8mb4&parseTime=True"
	SIGNER_URL      = "http://127.0.0.1:8081/internal/sign_tx"
	HOT_WALLET_ADDR = "0xYourHotWalletAddress..." // ❗必须在 Signer 数据库里有这个地址的私钥
)

// --- 数据库模型 ---
type UserWithdrawal struct {
	ID        uint
	ToAddress string
	Amount    float64
	Status    int
	TxHash    string
}

// --- Signer 接口请求/响应 ---
type SignTxReq struct {
	Address string `json:"address"`  // 用哪个地址签？(填热钱包地址)
	TxRLP   string `json:"tx_rlp"`   // 待签名交易数据的 RLP 编码
	ChainID int64  `json:"chain_id"` // 链ID (防止重放攻击)
}

type SignTxResp struct {
	SignedTx string `json:"signed_tx"` // 签名后的 RLP
	Error    string `json:"error"`
}

var db *gorm.DB
var client *ethclient.Client

func main() {
	// 1. 初始化 DB
	var err error
	db, err = gorm.Open(mysql.Open(DSN), &gorm.Config{})
	if err != nil {
		log.Fatal("DB connect error:", err)
	}

	// 2. 初始化以太坊客户端
	client, err = ethclient.Dial(RPC_URL)
	if err != nil {
		log.Fatal("RPC connect error:", err)
	}

	log.Println("🚀 提现 Worker 已启动，正在轮询数据库...")

	// 3. 开启轮询循环
	for {
		processWithdrawals()
		time.Sleep(5 * time.Second) // 每5秒检查一次
	}
}

// 负责把“业务单据”转化为“区块链交易”
func processWithdrawals() {
	// 1. 捞取任务：查找 Status=0 (待处理) 的提现单
	var tasks []UserWithdrawal
	// 为了演示，一次只取 5 条，防止拥堵
	if err := db.Where("status = ?", 0).Limit(5).Find(&tasks).Error; err != nil {
		log.Println("查询任务失败:", err)
		return
	}

	for _, task := range tasks {
		log.Printf("处理提现单 ID: %d, 金额: %f ETH", task.ID, task.Amount)

		// 2. 执行核心转账逻辑 (构造 -> 签名 -> 广播)
		txHash, err := sendHotWalletTx(task.ToAddress, task.Amount)

		if err != nil {
			log.Printf("❌ 提现失败 ID %d: %v", task.ID, err)
			// 策略：可以重试，或者标记为 -1 (需人工介入)
			// 这里简单跳过，等待下一次轮询（如果在生产环境，需要增加重试次数计数器）
			continue
		}

		// 3. 成功！更新数据库
		// 将状态改为 1 (已广播)，并记录 TxHash
		task.Status = 1
		task.TxHash = txHash
		db.Save(&task)

		log.Printf("✅ 提现已广播 ID %d, Hash: %s", task.ID, txHash)
	}
}

// 构造交易与远程签名 (sendHotWalletTx)
func sendHotWalletTx(toAddrStr string, amountEth float64) (string, error) {
	ctx := context.Background()

	// A. 准备数据
	fromAddr := common.HexToAddress(HOT_WALLET_ADDR)
	toAddr := common.HexToAddress(toAddrStr)

	// 金额转换: Float ETH -> BigInt Wei
	amountWei := new(big.Int)
	new(big.Float).Mul(big.NewFloat(amountEth), big.NewFloat(1e18)).Int(amountWei)

	// B. 获取链上参数
	// 1. Nonce (交易计数器)
	nonce, err := client.PendingNonceAt(ctx, fromAddr)
	if err != nil {
		return "", err
	}

	// 2. Gas Price (建议使用 SuggestGasTipCap + BaseFee 在 EIP-1559 中，这里用 Legacy 简化演示)
	gasPrice, err := client.SuggestGasPrice(ctx)
	if err != nil {
		return "", err
	}

	// 3. Chain ID (非常重要！)
	chainID, err := client.NetworkID(ctx)
	if err != nil {
		return "", err
	}

	// C. 构造【未签名】交易
	// 注意：这里没有私钥！我们只是在拼装数据包
	tx := types.NewTransaction(nonce, toAddr, amountWei, 21000, gasPrice, nil)

	// D. 🔥 请求 Signer 进行签名 🔥
	// 我们把未签名的交易发给 Signer，Signer 用私钥签好后还给我们
	signedTx, err := callSignerToSign(tx, chainID)
	if err != nil {
		return "", err
	}

	// E. 广播【已签名】交易
	err = client.SendTransaction(ctx, signedTx)
	if err != nil {
		return "", err
	}

	return signedTx.Hash().Hex(), nil
}

// 调用 Signer 的 HTTP 工具函数
func callSignerToSign(tx *types.Transaction, chainID *big.Int) (*types.Transaction, error) {
	// 1. 序列化未签名交易 (RLP编码)
	// 我们需要传输二进制数据，Hex 编码是最安全的传输方式
	txBytes, err := tx.MarshalBinary()
	if err != nil {
		return nil, err
	}

	reqBody := SignTxReq{
		Address: HOT_WALLET_ADDR, // 告诉 Signer 用这把钥匙签
		TxRLP:   hex.EncodeToString(txBytes),
		ChainID: chainID.Int64(),
	}

	jsonBody, _ := json.Marshal(reqBody)

	// 2. 发送 HTTP POST 请求
	resp, err := http.Post(SIGNER_URL, "application/json", bytes.NewBuffer(jsonBody))
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()

	// 3. 解析响应
	var res SignTxResp
	if err := json.NewDecoder(resp.Body).Decode(&res); err != nil {
		return nil, err
	}

	if res.Error != "" {
		return nil, log.New(log.Writer(), "", 0).Output(2, "Signer Refused: "+res.Error) // 模拟一个 Error
	}

	// 4. 反序列化【已签名】交易
	// Signer 返回的是一串 Hex，我们要把它变回 Go 的 Transaction 对象
	signedTxBytes, err := hex.DecodeString(res.SignedTx)
	if err != nil {
		return nil, err
	}

	signedTx := new(types.Transaction)
	if err := signedTx.UnmarshalBinary(signedTxBytes); err != nil {
		return nil, err
	}

	return signedTx, nil
}
