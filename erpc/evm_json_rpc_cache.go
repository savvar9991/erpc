package erpc

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"github.com/bytedance/sonic"
	"github.com/erpc/erpc/common"
	"github.com/erpc/erpc/data"
	"github.com/rs/zerolog"
)

type EvmJsonRpcCache struct {
	conn    data.Connector
	network *Network
	logger  *zerolog.Logger
}

const (
	JsonRpcCacheContext common.ContextKey = "jsonRpcCache"
)

func NewEvmJsonRpcCache(ctx context.Context, logger *zerolog.Logger, cfg *common.ConnectorConfig) (*EvmJsonRpcCache, error) {
	logger.Info().Msg("initializing evm json rpc cache...")
	err := populateDefaults(cfg)
	if err != nil {
		return nil, err
	}

	c, err := data.NewConnector(ctx, logger, cfg)
	if err != nil {
		return nil, err
	}

	// set TTL method overrides
	for _, cacheInfo := range cfg.Methods {
		if err := c.SetTTL(cacheInfo.Method, cacheInfo.TTL); err != nil {
			return nil, err
		}
	}

	// set non cacheable methods
	for _, cacheInfo := range cfg.NonCacheableMethods {
		logger.Debug().Str("method", cacheInfo.Method).Msg("configuring non-cacheable method")
		if err := c.IgnoreMethod(cacheInfo.Method); err != nil {
			return nil, err
		}
		logger.Debug().Str("method", cacheInfo.Method).Msg("successfully configured non-cacheable method")

	}

	return &EvmJsonRpcCache{
		conn:   c,
		logger: logger,
	}, nil
}

func (c *EvmJsonRpcCache) WithNetwork(network *Network) *EvmJsonRpcCache {
	network.Logger.Debug().Msgf("creating EvmJsonRpcCache")
	return &EvmJsonRpcCache{
		logger:  c.logger,
		conn:    c.conn,
		network: network,
	}
}

func (c *EvmJsonRpcCache) Get(ctx context.Context, req *common.NormalizedRequest) (*common.NormalizedResponse, error) {
	rpcReq, err := req.JsonRpcRequest()
	if err != nil {
		return nil, err
	}

	// Skip cache for ignored methods
	if c.conn.IsMethodIgnored(rpcReq.Method) {
		return nil, nil
	}

	hasTTL := c.conn.HasTTL(rpcReq.Method)

	blockRef, blockNumber, err := common.ExtractEvmBlockReferenceFromRequest(rpcReq)
	if err != nil {
		return nil, err
	}
	if blockRef == "" && blockNumber == 0 && !hasTTL {
		return nil, nil
	}
	if blockNumber != 0 {
		s, err := c.shouldCacheForBlock(blockNumber)
		if err == nil && !s {
			return nil, nil
		}
	}

	groupKey, requestKey, err := generateKeysForJsonRpcRequest(req, blockRef)
	if err != nil {
		return nil, err
	}

	var resultString string
	if blockRef != "*" {
		resultString, err = c.conn.Get(ctx, data.ConnectorMainIndex, groupKey, requestKey)
	} else {
		resultString, err = c.conn.Get(ctx, data.ConnectorReverseIndex, groupKey, requestKey)
	}
	if err != nil {
		return nil, err
	}

	if resultString == `""` || resultString == "null" || resultString == "[]" || resultString == "{}" {
		return nil, nil
	}

	jrr := &common.JsonRpcResponse{
		JSONRPC: rpcReq.JSONRPC,
		ID:      rpcReq.ID,
		Error:   nil,
		Result:  json.RawMessage(resultString),
	}

	return common.NewNormalizedResponse().
		WithRequest(req).
		WithFromCache(true).
		WithJsonRpcResponse(jrr), nil
}

func (c *EvmJsonRpcCache) Set(ctx context.Context, req *common.NormalizedRequest, resp *common.NormalizedResponse) error {
	rpcReq, err := req.JsonRpcRequest()
	if err != nil {
		return err
	}

	rpcResp, err := resp.JsonRpcResponse()
	if err != nil {
		return err
	}

	// Skip cache for ignored methods
	if c.conn.IsMethodIgnored(rpcReq.Method) {
		return nil
	}

	lg := c.logger.With().Str("networkId", req.NetworkId()).Str("method", rpcReq.Method).Logger()

	shouldCache, err := shouldCache(lg, req, resp, rpcReq, rpcResp)
	if !shouldCache || err != nil {
		return err
	}

	blockRef, blockNumber, err := common.ExtractEvmBlockReference(rpcReq, rpcResp)
	if err != nil {
		return err
	}

	hasTTL := c.conn.HasTTL(rpcReq.Method)

	if blockRef == "" && blockNumber == 0 && !hasTTL {
		// Do not cache if we can't resolve a block reference (e.g. latest block requests)
		lg.Debug().
			Str("blockRef", blockRef).
			Int64("blockNumber", blockNumber).
			Msg("will not cache the response because it has no block reference or block number")
		return nil
	}

	if !hasTTL {
		if blockRef == "" && blockNumber == 0 {
			// Do not cache if we can't resolve a block reference (e.g. latest block requests)
			lg.Debug().
				Str("blockRef", blockRef).
				Int64("blockNumber", blockNumber).
				Msg("will not cache the response because it has no block reference or block number")
			return nil
		}

		if blockNumber > 0 {
			s, e := c.shouldCacheForBlock(blockNumber)
			if !s || e != nil {
				lg.Debug().
					Err(e).
					Str("blockRef", blockRef).
					Int64("blockNumber", blockNumber).
					Interface("result", rpcResp.Result).
					Msg("will not cache the response because block is not finalized")
				return e
			}
		}
	}

	pk, rk, err := generateKeysForJsonRpcRequest(req, blockRef)
	if err != nil {
		return err
	}

	lg.Debug().
		Str("blockRef", blockRef).
		Str("primaryKey", pk).
		Str("rangeKey", rk).
		Int64("blockNumber", blockNumber).
		Interface("result", rpcResp.Result).
		Msg("caching the response")

	resultBytes, err := sonic.Marshal(rpcResp.Result)
	if err != nil {
		return err
	}

	ctx, cancel := context.WithTimeoutCause(ctx, 5*time.Second, errors.New("evm json-rpc cache driver timeout during set"))
	defer cancel()
	return c.conn.Set(ctx, pk, rk, string(resultBytes))
}

func shouldCache(
	lg zerolog.Logger,
	req *common.NormalizedRequest,
	resp *common.NormalizedResponse,
	rpcReq *common.JsonRpcRequest,
	rpcResp *common.JsonRpcResponse,
) (bool, error) {
	if resp == nil ||
		resp.IsObjectNull() ||
		resp.IsResultEmptyish() ||
		rpcResp == nil ||
		rpcResp.Result == nil ||
		rpcResp.Error != nil {
		ups := resp.Upstream()
		if ups != nil {
			upsCfg := ups.Config()
			if upsCfg.Evm != nil {
				if upsCfg.Evm.Syncing != nil && !*upsCfg.Evm.Syncing {
					blkNum, err := req.EvmBlockNumber()
					if err != nil && blkNum > 0 {
						ntw := req.Network()
						if ntw != nil {
							if fin, err := ntw.EvmIsBlockFinalized(blkNum); err != nil && fin {
								return fin, nil
							}
						}
					}
				}
			}
		}

		lg.Debug().Msg("skip caching because it has no result or has error and we cannot determine finality and sync-state")
		return false, nil
	}

	switch rpcReq.Method {
	case "eth_getTransactionByHash",
		"eth_getTransactionReceipt",
		"eth_getTransactionByBlockHashAndIndex",
		"eth_getTransactionByBlockNumberAndIndex":

		// When transactions are not yet included in a block blockNumber/blockHash is still unknown
		// For these transaction for now we will not cache the response, but still must be returned
		// to the client because they might be intentionally looking for pending txs.
		// Is there a reliable way to cache these and bust in-case of a reorg?
		blkRef, blkNum, err := common.ExtractEvmBlockReferenceFromResponse(rpcReq, rpcResp)
		if err != nil {
			lg.Error().Err(err).Msg("skip caching because error extracting block reference from response")
			return false, err
		}
		if blkRef == "" && blkNum == 0 {
			lg.Debug().Msg("skip caching because block number/hash is not yet available (unconfirmed tx?)")
			return false, nil
		}

		return true, nil
	}

	return true, nil
}

func (c *EvmJsonRpcCache) DeleteByGroupKey(ctx context.Context, groupKeys ...string) error {
	for _, groupKey := range groupKeys {
		err := c.conn.Delete(ctx, data.ConnectorMainIndex, groupKey, "")
		if err != nil {
			return err
		}
	}

	return nil
}

func (c *EvmJsonRpcCache) shouldCacheForBlock(blockNumber int64) (bool, error) {
	b, e := c.network.EvmIsBlockFinalized(blockNumber)
	return b, e
}

func generateKeysForJsonRpcRequest(req *common.NormalizedRequest, blockRef string) (string, string, error) {
	cacheKey, err := req.CacheHash()
	if err != nil {
		return "", "", err
	}

	if blockRef != "" {
		return fmt.Sprintf("%s:%s", req.NetworkId(), blockRef), cacheKey, nil
	} else {
		return fmt.Sprintf("%s:nil", req.NetworkId()), cacheKey, nil
	}
}

func populateDefaults(cfg *common.ConnectorConfig) error {
	switch cfg.Driver {
	case data.DynamoDBDriverName:
		if cfg.DynamoDB.Table == "" {
			cfg.DynamoDB.Table = "erpc_json_rpc_cache"
		}
		if cfg.DynamoDB.PartitionKeyName == "" {
			cfg.DynamoDB.PartitionKeyName = "groupKey"
		}
		if cfg.DynamoDB.RangeKeyName == "" {
			cfg.DynamoDB.RangeKeyName = "requestKey"
		}
		if cfg.DynamoDB.ReverseIndexName == "" {
			cfg.DynamoDB.ReverseIndexName = "idx_groupKey_requestKey"
		}
	case data.RedisDriverName:
		if cfg.Redis.Addr == "" {
			cfg.Redis.Addr = "localhost:6379"
		}
	case data.PostgreSQLDriverName:
		if cfg.PostgreSQL.ConnectionUri == "" {
			cfg.PostgreSQL.ConnectionUri = "postgres://erpc:erpc@localhost:5432/erpc"
		}
		if cfg.PostgreSQL.Table == "" {
			cfg.PostgreSQL.Table = "erpc_json_rpc_cache"
		}
	}

	return nil
}
