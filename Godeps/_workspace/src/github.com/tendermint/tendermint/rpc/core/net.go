package core

import (
	dbm "github.com/eris-ltd/eris-db/Godeps/_workspace/src/github.com/tendermint/tendermint/db"
	ctypes "github.com/eris-ltd/eris-db/Godeps/_workspace/src/github.com/tendermint/tendermint/rpc/core/types"
	sm "github.com/eris-ltd/eris-db/Godeps/_workspace/src/github.com/tendermint/tendermint/state"
	"github.com/eris-ltd/eris-db/Godeps/_workspace/src/github.com/tendermint/tendermint/types"
)

//-----------------------------------------------------------------------------

// cache the genesis state
var genesisState *sm.State

func Status() (*ctypes.ResponseStatus, error) {
	db := dbm.NewMemDB()
	if genesisState == nil {
		genesisState = sm.MakeGenesisState(db, genDoc)
	}
	genesisHash := genesisState.Hash()
	latestHeight := blockStore.Height()
	var (
		latestBlockMeta *types.BlockMeta
		latestBlockHash []byte
		latestBlockTime int64
	)
	if latestHeight != 0 {
		latestBlockMeta = blockStore.LoadBlockMeta(latestHeight)
		latestBlockHash = latestBlockMeta.Hash
		latestBlockTime = latestBlockMeta.Header.Time.UnixNano()
	}

	return &ctypes.ResponseStatus{
		Moniker:           config.GetString("moniker"),
		ChainID:           config.GetString("chain_id"),
		Version:           config.GetString("version"),
		GenesisHash:       genesisHash,
		PubKey:            privValidator.PubKey,
		LatestBlockHash:   latestBlockHash,
		LatestBlockHeight: latestHeight,
		LatestBlockTime:   latestBlockTime}, nil
}

//-----------------------------------------------------------------------------

func NetInfo() (*ctypes.ResponseNetInfo, error) {
	listening := p2pSwitch.IsListening()
	listeners := []string{}
	for _, listener := range p2pSwitch.Listeners() {
		listeners = append(listeners, listener.String())
	}
	peers := []ctypes.Peer{}
	for _, peer := range p2pSwitch.Peers().List() {
		peers = append(peers, ctypes.Peer{
			NodeInfo:   *peer.NodeInfo,
			IsOutbound: peer.IsOutbound(),
		})
	}
	return &ctypes.ResponseNetInfo{
		Listening: listening,
		Listeners: listeners,
		Peers:     peers,
	}, nil
}

//-----------------------------------------------------------------------------

func Genesis() (*sm.GenesisDoc, error) {
	return genDoc, nil
}