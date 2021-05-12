// Copyright (C) 2019-2021 Algorand, Inc.
// This file is part of go-algorand
//
// go-algorand is free software: you can redistribute it and/or modify
// it under the terms of the GNU Affero General Public License as
// published by the Free Software Foundation, either version 3 of the
// License, or (at your option) any later version.
//
// go-algorand is distributed in the hope that it will be useful,
// but WITHOUT ANY WARRANTY; without even the implied warranty of
// MERCHANTABILITY or FITNESS FOR A PARTICULAR PURPOSE.  See the
// GNU Affero General Public License for more details.
//
// You should have received a copy of the GNU Affero General Public License
// along with go-algorand.  If not, see <https://www.gnu.org/licenses/>.

package ledger

import (
	"fmt"

	"github.com/algorand/go-algorand/config"
	"github.com/algorand/go-algorand/data/basics"
	"github.com/algorand/go-algorand/data/bookkeeping"
	"github.com/algorand/go-algorand/data/transactions"
	"github.com/algorand/go-algorand/ledger/ledgercore"
	"github.com/algorand/go-algorand/protocol"
)

//   ___________________
// < cow = Copy On Write >
//   -------------------
//          \   ^__^
//           \  (oo)\_______
//              (__)\       )\/\
//                  ||----w |
//                  ||     ||

type roundCowParent interface {
	// lookout with rewards
	lookup(basics.Address) (ledgercore.PersistedAccountData, error)
	lookupCreatableData(basics.Address, basics.CreatableIndex, basics.CreatableType, bool, bool) (ledgercore.PersistedAccountData, error)
	checkDup(basics.Round, basics.Round, transactions.Txid, ledgercore.Txlease) error
	txnCounter() uint64
	getCreator(cidx basics.CreatableIndex, ctype basics.CreatableType) (basics.Address, bool, error)
	compactCertNext() basics.Round
	blockHdr(rnd basics.Round) (bookkeeping.BlockHeader, error)
	getStorageCounts(addr basics.Address, aidx basics.AppIndex, global bool) (basics.StateSchema, error)
	// note: getStorageLimits is redundant with the other methods
	// and is provided to optimize state schema lookups
	getStorageLimits(addr basics.Address, aidx basics.AppIndex, global bool) (basics.StateSchema, error)
	allocated(addr basics.Address, aidx basics.AppIndex, global bool) (bool, error)
	getKey(addr basics.Address, aidx basics.AppIndex, global bool, key string, accountIdx uint64) (basics.TealValue, bool, error)
}

type roundCowState struct {
	lookupParent roundCowParent
	commitParent *roundCowState
	proto        config.ConsensusParams
	mods         ledgercore.StateDelta

	// storage deltas populated as side effects of AppCall transaction
	// 1. Opt-in/Close actions (see Allocate/Deallocate)
	// 2. Stateful TEAL evaluation (see SetKey/DelKey)
	// must be incorporated into mods.accts before passing deltas forward
	sdeltas map[basics.Address]map[storagePtr]*storageDelta

	// getPadCache provides compatibility between mods that uses PersistedAccountData
	// and balances interface implementation (Get, GetEx, Put, PutWithCreatable)
	// that work with AccountData view to PersistedAccountData.
	// The idea is Getters populate getPadCache and return AccountData portion,
	// and Putters find corresponding PersistedAccountData there without looking into DB
	getPadCache map[basics.Address]ledgercore.PersistedAccountData

	// either or not maintain compatibility with original app refactoring behavior
	// this is needed for generating old eval delta in new code
	compatibilityMode bool
	// cache mainaining accountIdx used in getKey for local keys access
	compatibilityGetKeyCache map[basics.Address]map[storagePtr]uint64
}

func makeRoundCowState(b roundCowParent, hdr bookkeeping.BlockHeader, prevTimestamp int64, hint int) *roundCowState {
	cb := roundCowState{
		lookupParent: b,
		commitParent: nil,
		proto:        config.Consensus[hdr.CurrentProtocol],
		mods:         ledgercore.MakeStateDelta(&hdr, prevTimestamp, hint, 0),
		sdeltas:      make(map[basics.Address]map[storagePtr]*storageDelta),
		getPadCache:  make(map[basics.Address]ledgercore.PersistedAccountData),
	}

	// compatibilityMode retains producing application' eval deltas under the following rule:
	// local delta has account index as it specified in TEAL either in set/del key or prior get key calls.
	// The predicate is that complex in order to cover all the block seen on testnet and mainnet.
	compatibilityMode := (hdr.CurrentProtocol == protocol.ConsensusV24) &&
		(hdr.NextProtocol != protocol.ConsensusV26 || (hdr.UpgradePropose == "" && hdr.UpgradeApprove == false && hdr.Round < hdr.UpgradeState.NextProtocolVoteBefore))
	if compatibilityMode {
		cb.compatibilityMode = true
		cb.compatibilityGetKeyCache = make(map[basics.Address]map[storagePtr]uint64)
	}
	return &cb
}

func (cb *roundCowState) deltas() ledgercore.StateDelta {
	var err error
	if len(cb.sdeltas) == 0 {
		return cb.mods
	}

	// Apply storage deltas to account deltas
	// 1. Ensure all addresses from sdeltas have entries in accts because
	//    SetKey/DelKey work only with sdeltas, so need to pull missing accounts
	// 2. Call applyStorageDelta for every delta per account
	for addr, smap := range cb.sdeltas {
		var pad ledgercore.PersistedAccountData
		var exist bool
		if pad, exist = cb.mods.Accts.Get(addr); !exist {
			pad, err = cb.lookup(addr)
			if err != nil {
				panic(fmt.Sprintf("fetching account data failed for addr %s: %s", addr.String(), err.Error()))
			}
		}
		for aapp, storeDelta := range smap {
			if pad.AccountData, err = applyStorageDelta(pad.AccountData, aapp, storeDelta); err != nil {
				panic(fmt.Sprintf("applying storage delta failed for addr %s app %d: %s", addr.String(), aapp.aidx, err.Error()))
			}
		}
		cb.mods.Accts.Upsert(addr, pad)
	}
	return cb.mods
}

func (cb *roundCowState) rewardsLevel() uint64 {
	return cb.mods.Hdr.RewardsLevel
}

func (cb *roundCowState) round() basics.Round {
	return cb.mods.Hdr.Round
}

func (cb *roundCowState) prevTimestamp() int64 {
	return cb.mods.PrevTimestamp
}

func (cb *roundCowState) getCreator(cidx basics.CreatableIndex, ctype basics.CreatableType) (creator basics.Address, ok bool, err error) {
	delta, ok := cb.mods.Creatables[cidx]
	if ok {
		if delta.Created && delta.Ctype == ctype {
			return delta.Creator, true, nil
		}
		return basics.Address{}, false, nil
	}
	return cb.lookupParent.getCreator(cidx, ctype)
}

func (cb *roundCowState) lookup(addr basics.Address) (pad ledgercore.PersistedAccountData, err error) {
	pad, ok := cb.mods.Accts.Get(addr)
	if ok {
		return pad, nil
	}

	pad, err = cb.lookupParent.lookup(addr)
	if err != nil {
		return
	}

	// save PersistentAccountData for later usage in put
	cb.getPadCache[addr] = pad
	return
}

// lookupWithHolding is gets account data but also fetches asset holding or app local data for a specified creatable
func (cb *roundCowState) lookupCreatableData(addr basics.Address, cidx basics.CreatableIndex, ctype basics.CreatableType, global bool, local bool) (data ledgercore.PersistedAccountData, err error) {
	if !global && !local || ctype != basics.AssetCreatable && ctype != basics.AppCreatable {
		panic(fmt.Sprintf("lookupCreatableData/GetEx misuse: %d, %v, %v", ctype, global, local))
	}

	pad, modified := cb.mods.Accts.Get(addr)
	if modified {
		globalExist := false
		localExist := false
		if ctype == basics.AssetCreatable {
			if global {
				_, globalExist = pad.AccountData.AssetParams[basics.AssetIndex(cidx)]
			}
			if local {
				_, localExist = pad.AccountData.Assets[basics.AssetIndex(cidx)]
			}
		} else {
			if global {
				_, globalExist = pad.AccountData.AppParams[basics.AppIndex(cidx)]
			}
			if local {
				_, localExist = pad.AccountData.AppLocalStates[basics.AppIndex(cidx)]
			}
		}

		onlyGlobal := global && globalExist && !local
		onlyLocal := local && localExist && !global
		bothGlobalLocal := global && globalExist && local && localExist
		if onlyGlobal || onlyLocal || bothGlobalLocal {
			return pad, nil
		}
	}

	parentPad, err := cb.lookupParent.lookupCreatableData(addr, cidx, ctype, global, local)
	if !modified {
		cb.getPadCache[addr] = parentPad
		return parentPad, err
	}

	// data from cb.mods.Accts is newer than from lookupParent.lookupHolding, so add the asset if any
	if ctype == basics.AssetCreatable {
		if global {
			if params, ok := parentPad.AccountData.AssetParams[basics.AssetIndex(cidx)]; ok {
				pad.AccountData.AssetParams[basics.AssetIndex(cidx)] = params
			}
		}
		if local {
			if holding, ok := parentPad.AccountData.Assets[basics.AssetIndex(cidx)]; ok {
				pad.AccountData.Assets[basics.AssetIndex(cidx)] = holding
			}
		}
	} else {
		if global {
			if params, ok := parentPad.AccountData.AppParams[basics.AppIndex(cidx)]; ok {
				pad.AccountData.AppParams[basics.AppIndex(cidx)] = params
			}
		}
		if local {
			if states, ok := parentPad.AccountData.AppLocalStates[basics.AppIndex(cidx)]; ok {
				pad.AccountData.AppLocalStates[basics.AppIndex(cidx)] = states
			}
		}
	}

	cb.getPadCache[addr] = pad
	return pad, nil
}

func (cb *roundCowState) checkDup(firstValid, lastValid basics.Round, txid transactions.Txid, txl ledgercore.Txlease) error {
	_, present := cb.mods.Txids[txid]
	if present {
		return &ledgercore.TransactionInLedgerError{Txid: txid}
	}

	if cb.proto.SupportTransactionLeases && (txl.Lease != [32]byte{}) {
		expires, ok := cb.mods.Txleases[txl]
		if ok && cb.mods.Hdr.Round <= expires {
			return ledgercore.MakeLeaseInLedgerError(txid, txl)
		}
	}

	return cb.lookupParent.checkDup(firstValid, lastValid, txid, txl)
}

func (cb *roundCowState) txnCounter() uint64 {
	return cb.lookupParent.txnCounter() + uint64(len(cb.mods.Txids))
}

func (cb *roundCowState) compactCertNext() basics.Round {
	if cb.mods.CompactCertNext != 0 {
		return cb.mods.CompactCertNext
	}
	return cb.lookupParent.compactCertNext()
}

func (cb *roundCowState) blockHdr(r basics.Round) (bookkeeping.BlockHeader, error) {
	return cb.lookupParent.blockHdr(r)
}

func (cb *roundCowState) put(addr basics.Address, new basics.AccountData, newCreatable *basics.CreatableLocator, deletedCreatable *basics.CreatableLocator) {
	// convert AccountData to PersistentAccountData by using getPadCache
	// that is must be filled in lookup* methods
	if pad, ok := cb.getPadCache[addr]; ok {
		pad.AccountData = new
		cb.mods.Accts.Upsert(addr, pad)
	} else {
		panic(fmt.Sprintf("Address %s does not have entry in getPadCache", addr.String()))
	}

	if newCreatable != nil {
		cb.mods.Creatables[newCreatable.Index] = ledgercore.ModifiedCreatable{
			Ctype:   newCreatable.Type,
			Creator: newCreatable.Creator,
			Created: true,
		}
	}
	if deletedCreatable != nil {
		cb.mods.Creatables[deletedCreatable.Index] = ledgercore.ModifiedCreatable{
			Ctype:   deletedCreatable.Type,
			Creator: deletedCreatable.Creator,
			Created: false,
		}
	}
}

func (cb *roundCowState) addTx(txn transactions.Transaction, txid transactions.Txid) {
	cb.mods.Txids[txid] = txn.LastValid
	cb.mods.Txleases[ledgercore.Txlease{Sender: txn.Sender, Lease: txn.Lease}] = txn.LastValid
}

func (cb *roundCowState) setCompactCertNext(rnd basics.Round) {
	cb.mods.CompactCertNext = rnd
}

func (cb *roundCowState) child(hint int) *roundCowState {
	ch := roundCowState{
		lookupParent: cb,
		commitParent: cb,
		proto:        cb.proto,
		mods:         ledgercore.MakeStateDelta(cb.mods.Hdr, cb.mods.PrevTimestamp, hint, cb.mods.CompactCertNext),
		sdeltas:      make(map[basics.Address]map[storagePtr]*storageDelta),
		getPadCache:  make(map[basics.Address]ledgercore.PersistedAccountData),
	}

	if cb.compatibilityMode {
		ch.compatibilityMode = cb.compatibilityMode
		ch.compatibilityGetKeyCache = make(map[basics.Address]map[storagePtr]uint64)
	}
	return &ch
}

func (cb *roundCowState) commitToParent() {
	cb.commitParent.mods.Accts.MergeAccounts(cb.mods.Accts)

	for txid, lv := range cb.mods.Txids {
		cb.commitParent.mods.Txids[txid] = lv
	}
	for txl, expires := range cb.mods.Txleases {
		cb.commitParent.mods.Txleases[txl] = expires
	}
	for cidx, delta := range cb.mods.Creatables {
		cb.commitParent.mods.Creatables[cidx] = delta
	}
	for addr, smod := range cb.sdeltas {
		for aapp, nsd := range smod {
			lsd, ok := cb.commitParent.sdeltas[addr][aapp]
			if ok {
				lsd.applyChild(nsd)
			} else {
				_, ok = cb.commitParent.sdeltas[addr]
				if !ok {
					cb.commitParent.sdeltas[addr] = make(map[storagePtr]*storageDelta)
				}
				cb.commitParent.sdeltas[addr][aapp] = nsd
			}
		}
	}
	cb.commitParent.mods.CompactCertNext = cb.mods.CompactCertNext

	for addr, pad := range cb.getPadCache {
		cb.commitParent.getPadCache[addr] = pad
	}
}

func (cb *roundCowState) modifiedAccounts() []basics.Address {
	return cb.mods.Accts.ModifiedAccounts()
}
