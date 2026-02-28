package rebalance

import (
	"bytes"
	"context"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"log"
	"math/big"
	"net/http"
	"time"

	"github.com/ethereum/go-ethereum/common"
	"github.com/ethereum/go-ethereum/core/types"
	"github.com/ethereum/go-ethereum/ethclient"
	_ "github.com/go-sql-driver/mysql"
)

const (
	RPC_URL         = "https://mainnet.infura.io/v3/YOUR_KEY"
	DSN             = "root:123456@tcp(127.0.0.1:3306)/business_wallet?charset=utf8mb4&parseTime=True"
	SIGNER_URL      = "http://127.0.0.1:8081/internal/sign_tx" // 签名机接口
	SWEEP_THRESHOLD = 0.1                                      // 用户余额 > 0.1 ETH 才归集，防止亏损Gas费
)

var db *sql.DB
var client *ethclient.Client

// 缓存系统钱包地址
var hotWalletAddr string
var coldWalletAddr string

func main() {
	var err error
	db, err = sql.Open("mysql", DSN)
	if err != nil {
		log.Fatal(err)
	}

	client, err = ethclient.Dial(RPC_URL)
	if err != nil {
		log.Fatal(err)
	}

	// 加载配置
	loadSystemWallets()

	log.Println("资金调度服务启动 (Fund Rebalance)...")

	// 开启两个并发任务
	go sweepLoop()     // 任务1: 用户 -> 热钱包
	go rebalanceLoop() // 任务2: 热钱包 <-> 冷钱包

	select {} // 阻塞主进程
}

func loadSystemWallets() {
	// 从数据库加载 HOT 和 COLD 地址配置
	db.QueryRow("SELECT address FROM system_wallets WHERE wallet_type='HOT'").Scan(&hotWalletAddr)
	db.QueryRow("SELECT address FROM system_wallets WHERE wallet_type='COLD'").Scan(&coldWalletAddr)
	log.Printf("配置加载: Hot=%s, Cold=%s", hotWalletAddr, coldWalletAddr)
}

// 用户资金归集 (Sweep)
func sweepLoop() {
	ticker := time.NewTicker(1 * time.Minute)
	for range ticker.C {
		// 1. 查找数据库中余额大于阈值的用户 (预筛选)
		// 注意：这里用数据库余额做初步筛选，减少链上RPC请求
		rows, err := db.Query("SELECT user_id, address FROM user_accounts WHERE status=1") // 假设添加了余额字段或关联查询
		if err != nil {
			continue
		}

		for rows.Next() {
			var uid, addr string
			rows.Scan(&uid, &addr)
			processUserSweep(uid, addr)
		}
		rows.Close()
	}
}

func processUserSweep(userID, userAddr string) {
	ctx := context.Background()
	account := common.HexToAddress(userAddr)

	// 1. 获取链上实时余额
	balance, err := client.BalanceAt(ctx, account, nil)
	if err != nil {
		return
	}

	// 转换阈值 0.1 ETH 到 Wei
	thresholdWei := new(big.Int).Mul(big.NewInt(1e17), big.NewInt(1)) // 0.1 * 10^18

	if balance.Cmp(thresholdWei) <= 0 {
		return // 余额太少，不够Gas费或没必要归集
	}

	// 2. 估算Gas
	gasLimit := uint64(21000)
	gasPrice, _ := client.SuggestGasPrice(ctx)
	gasCost := new(big.Int).Mul(gasPrice, big.NewInt(int64(gasLimit)))

	// 3. 计算实际归集金额 (余额 - Gas费)
	amountToSend := new(big.Int).Sub(balance, gasCost)
	if amountToSend.Cmp(big.NewInt(0)) <= 0 {
		return
	}

	// 4. 构建未签名交易
	nonce, _ := client.PendingNonceAt(ctx, account)
	tx := types.NewTransaction(
		nonce,
		common.HexToAddress(hotWalletAddr), // 目标：热钱包
		amountToSend,
		gasLimit,
		gasPrice,
		nil,
	)

	// 5. 请求 Signer 签名 (这是关键的一步！私钥不在这里)
	signedTx, err := callSignerToSign(tx, userAddr) // 需要实现这个HTTP调用
	if err != nil {
		log.Println("归集签名失败:", err)
		return
	}

	// 6. 广播交易
	err = client.SendTransaction(ctx, signedTx)
	if err != nil {
		log.Println("归集广播失败:", err)
		return
	}

	log.Printf("归集成功! User: %s, Tx: %s, Amount: %s", userID, signedTx.Hash().Hex(), amountToSend.String())

	// 7. 记录日志
	db.Exec("INSERT INTO sweep_logs (user_id, from_address, to_address, amount, tx_hash) VALUES (?,?,?,?,?)",
		userID, userAddr, hotWalletAddr, amountToSend.String(), signedTx.Hash().Hex())
}

// 热钱包水位调度 (Rebalance)
func rebalanceLoop() {
	ticker := time.NewTicker(10 * time.Minute)
	for range ticker.C {
		checkHotWalletHealth()
	}
}

func checkHotWalletHealth() {
	ctx := context.Background()

	// 1. 检查热钱包余额
	balanceWei, _ := client.BalanceAt(ctx, common.HexToAddress(hotWalletAddr), nil)

	// 简单转换 float 便于比较
	balanceEth := new(big.Float).Quo(new(big.Float).SetInt(balanceWei), big.NewFloat(1e18))
	val, _ := balanceEth.Float64()

	// 读取配置
	var max, min float64
	db.QueryRow("SELECT balance_threshold_max, balance_threshold_min FROM system_wallets WHERE wallet_type='HOT'").Scan(&max, &min)

	if val > max {
		// --- 场景 A: 钱太多了 (安全风险) ---
		log.Printf("警报: 热钱包余额过高 (%f ETH)！触发自动归集到冷钱包...", val)

		// 动作: 将多余部分 (balance - max) 转入冷钱包
		// 逻辑同上：构建交易 -> Signer签名(热钱包私钥) -> 广播 -> 目标地址是 ColdWalletAddr
		triggerTransferToCold(val - max)

	} else if val < min {
		// --- 场景 B: 钱不够了 (提现阻塞风险) ---
		log.Printf("警报: 热钱包余额不足 (%f ETH)！请立即从冷钱包转账。", val)

		// 动作: 这是一个多签操作，程序无法自动完成。
		// 1. 发送 Slack/Telegram 报警给财务人员。
		// 2. (进阶) 调用 Gnosis Safe API 自动创建一个 "Pending" 的提现提案，等待管理员在界面上点击确认。
		sendAlertToAdmin(val)
	}
}

// 定义请求和响应结构体（需与 signer 服务保持一致）
type SignTxReq struct {
	Address string `json:"address"`
	TxRLP   string `json:"tx_rlp"`
	ChainID int64  `json:"chain_id"`
}

type SignTxResp struct {
	SignedTx string `json:"signed_tx"` // Hex string
	Error    string `json:"error"`
}

func callSignerToSign(tx *types.Transaction, fromAddr string) (*types.Transaction, error) {
	// 1. 获取 ChainID (主网是1, 测试网不同)
	chainID, err := client.NetworkID(context.Background())
	if err != nil {
		return nil, fmt.Errorf("failed to get chainID: %v", err)
	}

	// 2. 序列化交易 (RLP编码)
	txBytes, err := tx.MarshalBinary()
	if err != nil {
		return nil, fmt.Errorf("failed to marshal tx: %v", err)
	}
	txHex := hex.EncodeToString(txBytes)

	// 3. 构造请求 Payload
	reqBody := SignTxReq{
		Address: fromAddr,
		TxRLP:   txHex,
		ChainID: chainID.Int64(),
	}
	jsonBody, _ := json.Marshal(reqBody)

	// 4. 发送 HTTP 请求给 Signer 服务
	resp, err := http.Post(SIGNER_URL, "application/json", bytes.NewBuffer(jsonBody))
	if err != nil {
		return nil, fmt.Errorf("signer request failed: %v", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != 200 {
		return nil, fmt.Errorf("signer returned status: %d", resp.StatusCode)
	}

	// 5. 解析响应
	var res SignTxResp
	if err := json.NewDecoder(resp.Body).Decode(&res); err != nil {
		return nil, fmt.Errorf("failed to decode signer response: %v", err)
	}

	if res.Error != "" {
		return nil, fmt.Errorf("signer error: %s", res.Error)
	}

	// 6. 反序列化已签名的交易
	signedTxBytes, err := hex.DecodeString(res.SignedTx)
	if err != nil {
		return nil, fmt.Errorf("invalid hex from signer: %v", err)
	}

	signedTx := new(types.Transaction)
	if err := signedTx.UnmarshalBinary(signedTxBytes); err != nil {
		return nil, fmt.Errorf("failed to unmarshal signed tx: %v", err)
	}

	return signedTx, nil
}

func triggerTransferToCold(excessAmountEth float64) {
	ctx := context.Background()

	// 1. 转换金额 float64 -> BigInt (Wei)
	// 注意：float 转换可能会有微小精度丢失，生产环境建议全程用 big.Int
	amountWei := new(big.Int)
	amountFloat := new(big.Float).Mul(big.NewFloat(excessAmountEth), big.NewFloat(1e18))
	amountFloat.Int(amountWei) // 转换结果存入 amountWei

	// 2. 准备热钱包地址对象
	fromAddr := common.HexToAddress(hotWalletAddr)
	toAddr := common.HexToAddress(coldWalletAddr)

	// 3. 估算 Gas 和构建交易
	nonce, err := client.PendingNonceAt(ctx, fromAddr)
	if err != nil {
		log.Println("获取热钱包 Nonce 失败:", err)
		return
	}

	gasLimit := uint64(21000)
	gasPrice, _ := client.SuggestGasPrice(ctx)

	// 扣除 Gas 费 (如果是全部转出需要扣，如果是转出部分则不需要扣，这里假设转出 excessAmount)
	// 为了简单，我们直接转 excessAmount，假设热钱包里还有剩余的钱付 Gas
	tx := types.NewTransaction(nonce, toAddr, amountWei, gasLimit, gasPrice, nil)

	// 4. 请求 Signer 签名 (用热钱包的私钥签)
	signedTx, err := callSignerToSign(tx, hotWalletAddr)
	if err != nil {
		log.Println("热转冷签名失败:", err)
		return
	}

	// 5. 广播
	err = client.SendTransaction(ctx, signedTx)
	if err != nil {
		log.Println("热转冷广播失败:", err)
		return
	}

	log.Printf("资金调配完成: 热钱包 -> 冷钱包, 金额: %f ETH, Hash: %s", excessAmountEth, signedTx.Hash().Hex())
}

func sendAlertToAdmin(currentBalance float64) {
	msg := fmt.Sprintf("【严重警报】热钱包余额不足！当前余额: %.4f ETH。请立即从冷钱包转账到: %s", currentBalance, hotWalletAddr)

	// 1. 控制台强提醒
	log.Println("========================================")
	log.Println(msg)
	log.Println("========================================")

	// 2. (可选) 发送 Slack/钉钉 消息
	// go sendSlackNotification(msg)
}

// 这是一个对接 Slack Webhook 的示例 (需要你自己申请 Webhook URL)
/*
func sendSlackNotification(msg string) {
	webhookURL := "https://hooks.slack.com/services/YOUR/WEBHOOK/URL"
	payload := map[string]string{"text": msg}
	jsonPayload, _ := json.Marshal(payload)

	http.Post(webhookURL, "application/json", bytes.NewBuffer(jsonPayload))
}
*/
