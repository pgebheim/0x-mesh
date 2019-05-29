package zeroex

import (
	"context"
	"math/big"
	"sync"
	"time"

	"github.com/0xProject/0x-mesh/constants"
	"github.com/0xProject/0x-mesh/ethereum/wrappers"
	"github.com/ethereum/go-ethereum/accounts/abi/bind"
	"github.com/ethereum/go-ethereum/common"
	"github.com/ethereum/go-ethereum/ethclient"
	"github.com/jpillora/backoff"
	log "github.com/sirupsen/logrus"
)

// MainnetOrderValidatorAddress is the mainnet OrderValidator contract address
var MainnetOrderValidatorAddress = common.HexToAddress("0x9463e518dea6810309563c81d5266c1b1d149138")

// GanacheOrderValidatorAddress is the ganache snapshot OrderValidator contract address
var GanacheOrderValidatorAddress = common.HexToAddress("0x32eecaf51dfea9618e9bc94e9fbfddb1bbdcba15")

// The most orders we can validate in a single eth_call without having the request timeout
const chunkSize = 500

// The context timeout length to use for requests to getOrdersAndTradersInfoTimeout
const getOrdersAndTradersInfoTimeout = 15 * time.Second

// Specifies the max number of eth_call requests we want to make concurrently.
// Additional requests will block until an ongoing request has completed.
const concurrencyLimit = 5

type getOrdersAndTradersInfoParams struct {
	TakerAddresses []common.Address
	Orders         []wrappers.OrderWithoutExchangeAddress
	Signatures     [][]byte
}

// OrderInfo represents the order information emitted from Mesh
type OrderInfo struct {
	OrderHash                common.Hash
	SignedOrder              *SignedOrder
	FillableTakerAssetAmount *big.Int
	OrderStatus              OrderStatus
	// The hash of the Ethereum transaction that caused the order status to change
	TxHash common.Hash
}

// SuccinctOrderInfo represents the necessary information about an order without including
// the order itself
type SuccinctOrderInfo struct {
	OrderHash                common.Hash
	FillableTakerAssetAmount *big.Int
	OrderStatus              OrderStatus
}

// OrderHashToSuccinctOrderInfo maps orderHashes to their corresponding succinctOrderInfo
type OrderHashToSuccinctOrderInfo map[common.Hash]*SuccinctOrderInfo

// OrderValidator validates 0x orders
type OrderValidator struct {
	orderValidator   *wrappers.OrderValidator
	assetDataDecoder *AssetDataDecoder
}

// NewOrderValidator instantiates a new order validator
func NewOrderValidator(ethClient *ethclient.Client, networkID int) (*OrderValidator, error) {
	contractNameToAddress := constants.NetworkIDToContractAddresses[networkID]
	orderValidator, err := wrappers.NewOrderValidator(contractNameToAddress.OrderValidator, ethClient)
	if err != nil {
		return nil, err
	}

	assetDataDecoder, err := NewAssetDataDecoder()
	if err != nil {
		return nil, err
	}

	return &OrderValidator{
		orderValidator:   orderValidator,
		assetDataDecoder: assetDataDecoder,
	}, nil
}

// BatchValidate retrieves all the information needed to validate the supplied orders.
// It splits the orders into chunks of `chunkSize`, and makes no more then `concurrencyLimit`
// requests concurrently. If a request fails, re-attempt it up to four times before giving up.
// If it some requests fail, this method still returns whatever order information it was able to
// retrieve.
func (o *OrderValidator) BatchValidate(rawSignedOrders []*SignedOrder) map[common.Hash]*OrderInfo {
	if len(rawSignedOrders) == 0 {
		return map[common.Hash]*OrderInfo{}
	}
	orderHashToInfo, schemaCheckedSignedOrders := o.BatchOffchainValidation(rawSignedOrders)
	takerAddresses := []common.Address{}
	for _, signedOrder := range schemaCheckedSignedOrders {
		takerAddresses = append(takerAddresses, signedOrder.TakerAddress)
	}
	orders := []wrappers.OrderWithoutExchangeAddress{}
	for _, signedOrder := range schemaCheckedSignedOrders {
		orders = append(orders, signedOrder.ConvertToOrderWithoutExchangeAddress())
	}
	signatures := [][]byte{}
	for _, signedOrder := range schemaCheckedSignedOrders {
		signatures = append(signatures, signedOrder.Signature)
	}

	// Chunk into groups of chunkSize orders/takerAddresses for each call
	chunks := []getOrdersAndTradersInfoParams{}
	for len(orders) > chunkSize {
		chunks = append(chunks, getOrdersAndTradersInfoParams{
			TakerAddresses: takerAddresses[:chunkSize],
			Orders:         orders[:chunkSize],
			Signatures:     signatures[:chunkSize],
		})
		takerAddresses = takerAddresses[chunkSize:]
		orders = orders[chunkSize:]
		signatures = signatures[chunkSize:]
	}
	if len(orders) > 0 {
		chunks = append(chunks, getOrdersAndTradersInfoParams{
			TakerAddresses: takerAddresses,
			Orders:         orders,
			Signatures:     signatures,
		})
	}

	semaphoreChan := make(chan struct{}, concurrencyLimit)
	defer close(semaphoreChan)

	wg := &sync.WaitGroup{}
	for i, params := range chunks {
		wg.Add(1)
		go func(params getOrdersAndTradersInfoParams, i int) {
			defer wg.Done()

			// Add one to the semaphore chan. If it already has concurrencyLimit values,
			// the request blocks here until one frees up.
			semaphoreChan <- struct{}{}

			// Attempt to make the eth_call request 4 times with an exponential back-off.
			maxDuration := 4 * time.Second
			b := &backoff.Backoff{
				Min:    250 * time.Millisecond, // First back-off length
				Max:    maxDuration,            // Longest back-off length
				Factor: 2,                      // Factor to multiple each successive back-off
			}

			for {
				// Pass a context with a 15 second timeout to `GetOrdersAndTradersInfo` in order to avoid
				// any one request from taking longer then 15 seconds
				ctx, cancel := context.WithTimeout(context.Background(), getOrdersAndTradersInfoTimeout)
				defer cancel()
				opts := &bind.CallOpts{
					Pending: false,
					Context: ctx,
				}
				results, err := o.orderValidator.GetOrdersAndTradersInfo(opts, params.Orders, params.Signatures, params.TakerAddresses)
				if err != nil {
					log.WithFields(log.Fields{
						"error":     err.Error(),
						"attempt":   b.Attempt(),
						"numOrders": len(params.Orders),
					}).Info("GetOrdersAndTradersInfo request failed")
					d := b.Duration()
					if d == maxDuration {
						<-semaphoreChan
						log.WithFields(log.Fields{
							"error":     err.Error(),
							"numOrders": len(params.Orders),
						}).Warning("Gave up on GetOrdersAndTradersInfo request after backoff limit reached")
						return // Give up after 4 attempts
					}
					time.Sleep(d)
					continue
				}

				for j, orderInfo := range results.OrdersInfo {
					traderInfo := results.TradersInfo[j]
					isValidSignature := results.IsValidSignature[j]
					orderHash := common.Hash(orderInfo.OrderHash)
					signedOrder := schemaCheckedSignedOrders[chunkSize*i+j]
					orderStatus := OrderStatus(orderInfo.OrderStatus)
					if !isValidSignature {
						orderStatus = SignatureInvalid
					}
					switch orderStatus {
					// TODO(fabio): A future optimization would be to check that both the maker & taker
					// amounts are non-zero locally rather then wait for the RPC call to catch these two
					// failure cases.
					case InvalidMakerAssetAmount, InvalidTakerAssetAmount, Expired, FullyFilled, Cancelled, SignatureInvalid:
						orderHashToInfo[orderHash] = &OrderInfo{
							OrderHash:                orderHash,
							SignedOrder:              signedOrder,
							FillableTakerAssetAmount: big.NewInt(0),
							OrderStatus:              orderStatus,
						}
						continue
					case Fillable:
						orderHashToInfo[orderHash] = &OrderInfo{
							OrderHash:                orderHash,
							SignedOrder:              signedOrder,
							FillableTakerAssetAmount: calculateRemainingFillableTakerAmount(signedOrder, orderInfo, traderInfo),
							OrderStatus:              orderStatus,
						}
						continue
					}
				}

				<-semaphoreChan
				return
			}
		}(params, i)
	}

	wg.Wait()
	return orderHashToInfo
}

// BatchOffchainValidation performs all off-chain validation checks on a batch of 0x orders.
// These checks include:
// - `MakerAssetAmount` and `TakerAssetAmount` cannot be 0
// - `AssetData` fields contain properly encoded, and currently supported assetData (ERC20 & ERC721 for now)
// - `Signature` contains a properly encoded 0x signature
// - Validate that order isn't expired
// Returns an orderHashToInfo mapping with all invalid orders added to it, and an array of the valid signedOrders
func (o *OrderValidator) BatchOffchainValidation(signedOrders []*SignedOrder) (map[common.Hash]*OrderInfo, []*SignedOrder) {
	orderHashToInfo := map[common.Hash]*OrderInfo{}
	validSignedOrders := []*SignedOrder{}
	for _, signedOrder := range signedOrders {
		orderHash, err := signedOrder.ComputeOrderHash()
		if err != nil {
			log.Panic("Computing the orderHash failed unexpectedly")
		}
		now := big.NewInt(time.Now().Unix())
		if signedOrder.ExpirationTimeSeconds.Cmp(now) == -1 {
			orderHashToInfo[orderHash] = &OrderInfo{
				OrderHash:                orderHash,
				SignedOrder:              signedOrder,
				FillableTakerAssetAmount: big.NewInt(0),
				OrderStatus:              Expired,
			}
			continue
		}

		if signedOrder.MakerAssetAmount.Cmp(big.NewInt(0)) == 0 {
			orderHashToInfo[orderHash] = &OrderInfo{
				OrderHash:                orderHash,
				SignedOrder:              signedOrder,
				FillableTakerAssetAmount: big.NewInt(0),
				OrderStatus:              InvalidMakerAssetAmount,
			}
			continue
		}
		if signedOrder.TakerAssetAmount.Cmp(big.NewInt(0)) == 0 {
			orderHashToInfo[orderHash] = &OrderInfo{
				OrderHash:                orderHash,
				SignedOrder:              signedOrder,
				FillableTakerAssetAmount: big.NewInt(0),
				OrderStatus:              InvalidTakerAssetAmount,
			}
			continue
		}

		isMakerAssetDataSupported := o.isSupportedAssetData(signedOrder.MakerAssetData)
		if !isMakerAssetDataSupported {
			orderHashToInfo[orderHash] = &OrderInfo{
				OrderHash:                orderHash,
				SignedOrder:              signedOrder,
				FillableTakerAssetAmount: big.NewInt(0),
				OrderStatus:              InvalidMakerAssetData,
			}
			continue
		}
		isTakerAssetDataSupported := o.isSupportedAssetData(signedOrder.TakerAssetData)
		if !isTakerAssetDataSupported {
			orderHashToInfo[orderHash] = &OrderInfo{
				OrderHash:                orderHash,
				SignedOrder:              signedOrder,
				FillableTakerAssetAmount: big.NewInt(0),
				OrderStatus:              InvalidTakerAssetData,
			}
			continue
		}

		isSupportedSignature := isSupportedSignature(signedOrder.Signature, orderHash)
		if !isSupportedSignature {
			orderHashToInfo[orderHash] = &OrderInfo{
				OrderHash:                orderHash,
				SignedOrder:              signedOrder,
				FillableTakerAssetAmount: big.NewInt(0),
				OrderStatus:              SignatureInvalid,
			}
			continue
		}

		validSignedOrders = append(validSignedOrders, signedOrder)
	}

	return orderHashToInfo, validSignedOrders
}

func (o *OrderValidator) isSupportedAssetData(assetData []byte) bool {
	assetDataName, err := o.assetDataDecoder.GetName(assetData)
	if err != nil {
		return false
	}
	switch assetDataName {
	case "ERC20Token":
		var decodedAssetData ERC20AssetData
		err := o.assetDataDecoder.Decode(assetData, &decodedAssetData)
		if err != nil {
			return false
		}
	case "ERC721Token":
		var decodedAssetData ERC721AssetData
		err := o.assetDataDecoder.Decode(assetData, &decodedAssetData)
		if err != nil {
			return false
		}
	case "MultiAsset":
		// TODO(fabio): Once OrderValidator.sol supports validating orders involving multiAssetData,
		// refactor this to add support.
		return false
	default:
		return false
	}
	return true
}

func isSupportedSignature(signature []byte, orderHash common.Hash) bool {
	signatureType := SignatureType(signature[len(signature)-1])

	switch signatureType {
	case IllegalSignature:
	case InvalidSignature:
		return false

	case EIP712Signature:
		if len(signature) != 66 {
			return false
		}
		// TODO(fabio): Do further validation by splitting into r,s,v and do ECRecover

	case EthSignSignature:
		if len(signature) != 66 {
			return false
		}
		// TODO(fabio): Do further validation by splitting into r,s,v, add prefix to hash
		// and do ECRecover

	case ValidatorSignature:
		if len(signature) < 21 {
			return false
		}

	case WalletSignature:
	case PreSignedSignature:
		return true

	default:
		return false

	}

	return true
}

func calculateRemainingFillableTakerAmount(signedOrder *SignedOrder, orderInfo wrappers.OrderInfo, traderInfo wrappers.TraderInfo) *big.Int {
	minSet := []*big.Int{}

	// Calculate min of balance & allowance of makers makerAsset -> translate into takerAsset amount
	var maxMakerAssetFillAmount *big.Int
	if traderInfo.MakerBalance.Cmp(traderInfo.MakerAllowance) == -1 {
		maxMakerAssetFillAmount = traderInfo.MakerBalance
	} else {
		maxMakerAssetFillAmount = traderInfo.MakerAllowance
	}
	maxTakerAssetFillAmountGivenMakerConstraints := new(big.Int).Div(new(big.Int).Mul(maxMakerAssetFillAmount, signedOrder.TakerAssetAmount), signedOrder.MakerAssetAmount)

	minSet = append(minSet, maxTakerAssetFillAmountGivenMakerConstraints)

	// Calculate min of balance & allowance of maker's ZRX -> translate into takerAsset amount
	if signedOrder.MakerFee.Cmp(big.NewInt(0)) != 0 {
		var makerZRXAvailable *big.Int
		if traderInfo.MakerZrxBalance.Cmp(traderInfo.MakerZrxAllowance) == -1 {
			makerZRXAvailable = traderInfo.MakerZrxBalance
		} else {
			makerZRXAvailable = traderInfo.MakerZrxAllowance
		}
		maxTakerAssetFillAmountGivenMakerZRXConstraints := new(big.Int).Div(new(big.Int).Mul(makerZRXAvailable, signedOrder.TakerAssetAmount), signedOrder.MakerFee)
		minSet = append(minSet, maxTakerAssetFillAmountGivenMakerZRXConstraints)
	}

	// Add the remaining takerAsset fill amount to the minSet
	remainingTakerAssetFillAmount := new(big.Int).Sub(signedOrder.TakerAssetAmount, orderInfo.OrderTakerAssetFilledAmount)
	minSet = append(minSet, remainingTakerAssetFillAmount)

	var maxTakerAssetFillAmount *big.Int
	for _, minVal := range minSet {
		if maxTakerAssetFillAmount == nil || maxTakerAssetFillAmount.Cmp(minVal) != -1 {
			maxTakerAssetFillAmount = minVal
		}
	}

	return maxTakerAssetFillAmount
}

// IsOrderValid returns true if the OrderStatus is Fillable and the
// FillableTakerAssetAmount is greater than 0, indicating that the order is
// valid and can be filled. It returns false otherwise. Note that this only
// considers the given OrderInfo and does not update it or send any calls to
// Ethereum. Typically, you will need to call BatchValidate periodically in
// order to get the latest OrderInfo.
func IsOrderValid(orderInfo *OrderInfo) bool {
	return orderInfo.OrderStatus == Fillable && orderInfo.FillableTakerAssetAmount.Cmp(big.NewInt(0)) == 1
}