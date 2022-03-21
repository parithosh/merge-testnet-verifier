package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"math/bits"
	"net/http"
	"strings"
	"sync"
	"time"

	"github.com/ethereum/go-ethereum/common"
)

var (
	// Info Retrieval Global Timeout
	InfoRetrievalTimeout = 61 * time.Second
)

type BeaconClient struct {
	BaseURL    string
	HTTPClient *http.Client

	// Spec config
	Spec Spec

	// Genesis
	GenesisTime *uint64

	// Merge related
	TTD           TTD
	TTDSlotNumber *uint64

	// Merge Related
	TTDTimestamp *uint64

	// Lock
	l sync.Mutex

	// Context related
	lastCtx    context.Context
	lastCancel context.CancelFunc
}

func (cl *BeaconClient) ClientType() ClientType {
	return Beacon
}

func (cl *BeaconClient) UpdateTTDTimestamp(newTimestamp uint64) {
	timestamp := newTimestamp
	cl.TTDTimestamp = &timestamp
}

func (cl *BeaconClient) GetGenesisTime() *uint64 {
	if cl.GenesisTime == nil {
		res := GenesisResponse{}
		if err := cl.sendRequest(GET_REQUEST, V1_BEACON_GENESIS_ENDPOINT, &res); err == nil {
			genesisTime := res.GenesisTime
			cl.GenesisTime = &genesisTime
		}
	}
	return cl.GenesisTime
}

func (cl *BeaconClient) SlotAtTime(t uint64) (uint64, error) {
	genesisTimeP := cl.GetGenesisTime()
	if genesisTimeP == nil {
		return 0, fmt.Errorf("No genesis yet")
	}
	genesisTime := *genesisTimeP
	if genesisTime > t {
		return 0, fmt.Errorf("time before genesis")
	}
	return (t - genesisTime) / cl.Spec.SecondsPerSlot, nil
}
func (cl *BeaconClient) GetOngoingSlotNumber() (uint64, error) {
	return cl.SlotAtTime(uint64(time.Now().Unix()))
}

func (cl *BeaconClient) GetLatestBlockSlotNumber() (uint64, error) {
	return cl.GetOngoingSlotNumber()
}

func (cl *BeaconClient) UpdateGetTTDBlockSlot() (*uint64, error) {
	// We need to have the TTD block timestamp from the Execution Clients
	if cl.TTDSlotNumber != nil {
		return cl.TTDSlotNumber, nil
	}
	if cl.TTDTimestamp != nil {
		slotAtTTD, err := cl.SlotAtTime(*cl.TTDTimestamp)
		if err != nil {
			fmt.Printf("Error getting slot at time: %v\n", err)
			return nil, err
		}
		slot := slotAtTTD
		cl.TTDSlotNumber = &slot
		return cl.TTDSlotNumber, nil
	}
	return nil, nil
}

func (cl *BeaconClient) GetBeaconBlock(slotNumber uint64) (*BeaconBlockResponse, error) {
	resp := BeaconBlockResponse{}
	err := cl.sendRequest(GET_REQUEST, fmt.Sprintf(V1_BEACON_HEADERS_ENDPOINT, slotNumber), &resp)
	return &resp, err
}

func (cl *BeaconClient) GetFinalityCheckpoints(slotNumber uint64) (*StateFinalityCheckpoints, error) {
	resp := StateFinalityCheckpoints{}
	if err := cl.sendRequest(GET_REQUEST, fmt.Sprintf(V1_BEACON_STATE_FINALITY_CHECKPOINTS_ENDPOINT, slotNumber), &resp); err != nil {
		return nil, err
	}
	return &resp, nil
}

func (cl *BeaconClient) GetSlotCommittees(slotNumber uint64) (*[]Committee, error) {
	committees := make([]Committee, 0)
	var allCommittees []Committee
	if err := cl.sendRequest(GET_REQUEST, fmt.Sprintf(V1_BEACON_STATE_COMMITTEES_ENDPOINT, slotNumber), &allCommittees); err != nil {
		return nil, err
	}
	for _, c := range allCommittees {
		if c.Slot == slotNumber {
			c := c
			committees = append(committees, c)
		}
	}
	return &committees, nil
}

func (cl *BeaconClient) GetSlotCommitteeSize(slotNumber uint64) (uint64, error) {
	slotCommittees, err := cl.GetSlotCommittees(slotNumber)
	if err != nil {
		fmt.Printf("Error getting Slot Committees: %v\n", err)
		return 0, err
	}
	committeeCount := uint64(0)
	for _, sc := range *slotCommittees {
		committeeCount += uint64(len(sc.Validators))
	}
	return committeeCount, nil
}

func (cl *BeaconClient) GetAttestationsAtBlock(blockNumber uint64) (*[]Attestation, error) {
	var allAttestations []Attestation
	if err := cl.sendRequest(GET_REQUEST, fmt.Sprintf(V1_BEACON_BLOCKS_ATTESTATIONS_ENDPOINT, blockNumber), &allAttestations); err != nil {
		return nil, err
	}
	return &allAttestations, nil
}

func (cl *BeaconClient) GetAttestationCountForSlot(slotNumber uint64) (uint64, error) {
	timeout := time.After(InfoRetrievalTimeout)
	lastVerifiedBlock := slotNumber
	for {
		latestSlot, _ := cl.GetLatestBlockSlotNumber()
		for latestSlot > lastVerifiedBlock {
			attBlock, err := cl.GetAttestationsAtBlock(lastVerifiedBlock + 1)
			if err != nil {
				break
			}
			for _, att := range *attBlock {
				if att.Data.Slot == slotNumber {
					// we got the attestations
					attCount := uint64(bits.OnesCount64(att.AggregationBits))
					if attCount > 0 {
						attCount -= 1
					}
					return attCount, nil
				}
			}
			lastVerifiedBlock++
		}

		select {
		case <-time.After(time.Second):
		case <-timeout:
			return 0, fmt.Errorf("Timeout waiting for attestation count")
		}

	}
}

func (cl *BeaconClient) GetDataPoint(dataName MetricName, slotNumber uint64) (interface{}, error) {
	for {
		// We fetch information only for previous slots, not current ongoing slot
		ongoingSlot, _ := cl.GetOngoingSlotNumber()
		if slotNumber >= ongoingSlot {
			time.Sleep(time.Second)
		} else {
			break
		}
	}
	switch dataName {
	case SlotBlock:
		_, err := cl.GetBeaconBlock(slotNumber)
		if err == nil {
			return uint64(1), nil
		}
		return uint64(0), nil
	case FinalizedEpoch:
		// Return `1` for each Finalized root change
		if slotNumber == 0 {
			return uint64(0), nil
		}
		if (slotNumber % cl.Spec.SlotsPerEpoch) != 0 {
			return uint64(0), nil
		}

		currentSlotFinalityCheckpoint, err := cl.GetFinalityCheckpoints(slotNumber)
		if err != nil {
			return nil, err
		}
		zerosHash := common.Hash{}
		if currentSlotFinalityCheckpoint.Finalized.Root == zerosHash {
			return uint64(0), nil
		}

		prevSlotFinalityCheckpoint, err := cl.GetFinalityCheckpoints(slotNumber - 1)
		if err != nil {
			return nil, err
		}
		if prevSlotFinalityCheckpoint.Finalized.Root != currentSlotFinalityCheckpoint.Finalized.Root {
			return uint64(1), nil
		}
		return uint64(0), nil

	case JustifiedEpoch:
		// Return `1` for each Justified root change
		if slotNumber == 0 {
			return uint64(0), nil
		}
		if (slotNumber % cl.Spec.SlotsPerEpoch) != 0 {
			return uint64(0), nil
		}

		currentSlotFinalityCheckpoint, err := cl.GetFinalityCheckpoints(slotNumber)
		if err != nil {
			return nil, err
		}
		zerosHash := common.Hash{}
		if currentSlotFinalityCheckpoint.Justified.Root == zerosHash {
			return uint64(0), nil
		}

		prevSlotFinalityCheckpoint, err := cl.GetFinalityCheckpoints(slotNumber - 1)
		if err != nil {
			return nil, err
		}
		if prevSlotFinalityCheckpoint.Justified.Root != currentSlotFinalityCheckpoint.Justified.Root {
			return uint64(1), nil
		}
		return uint64(0), nil

	case SlotAttestations:
		slotAttestations, err := cl.GetAttestationCountForSlot(slotNumber)
		if err != nil {
			return slotAttestations, err
		}
		return slotAttestations, nil
	case SlotAttestationsPercentage:
		committeeSize, err := cl.GetSlotCommitteeSize(slotNumber)
		if err != nil {
			return uint64(0), err
		}
		if committeeSize == 0 {
			return committeeSize, fmt.Errorf("Empty committee for slot %d", slotNumber)
		}
		slotAttestations, err := cl.GetAttestationCountForSlot(slotNumber)
		if err != nil {
			return uint64(0), err
		}
		perc := (slotAttestations * 100) / committeeSize
		return perc, nil
	}

	return nil, fmt.Errorf("Invalid data name: %s", dataName)
}

func (cl *BeaconClient) Ctx() context.Context {
	if cl.lastCtx != nil {
		cl.lastCancel()
	}
	cl.lastCtx, cl.lastCancel = context.WithTimeout(context.Background(), 10*time.Second)
	return cl.lastCtx
}

type errorResponse struct {
	Code    int    `json:"code"`
	Message string `json:"message"`
}

type successResponse struct {
	Code int         `json:"code"`
	Data interface{} `json:"data"`
}

func (cl *BeaconClient) sendRequest(requestType string, requestEndPoint string, v interface{}) error {
	cl.l.Lock()
	defer cl.l.Unlock()
	req, err := http.NewRequest(requestType, fmt.Sprintf("%s%s", cl.BaseURL, requestEndPoint), nil)
	if err != nil {
		return err
	}

	req = req.WithContext(cl.Ctx())

	req.Header.Set("Content-Type", "application/json; charset=utf-8")
	req.Header.Set("Accept", "application/json; charset=utf-8")

	res, err := cl.HTTPClient.Do(req)
	if err != nil {
		return err
	}

	defer res.Body.Close()

	if res.StatusCode < http.StatusOK || res.StatusCode >= http.StatusBadRequest {
		var errRes errorResponse
		if err = json.NewDecoder(res.Body).Decode(&errRes); err == nil {
			return errors.New(errRes.Message)
		}

		return fmt.Errorf("unknown error, status code: %d", res.StatusCode)
	}

	fullResponse := successResponse{
		Data: v,
	}
	if err = json.NewDecoder(res.Body).Decode(&fullResponse); err != nil {
		return err
	}

	return nil
}

type BeaconClients []*BeaconClient

func (cls *BeaconClients) BaseURLs() *[]string {
	baseURLs := make([]string, 0)
	for _, el := range *cls {
		baseURLs = append(baseURLs, el.BaseURL)
	}
	return &baseURLs
}

func (cls *BeaconClients) String() string {
	return strings.Join(*cls.BaseURLs(), ",")
}

func (cls *BeaconClients) Set(baseUrl string) error {
	client := &http.Client{}

	if baseUrl[len(baseUrl)-1:] == "/" {
		baseUrl = baseUrl[:len(baseUrl)-1]
	}

	cl := BeaconClient{
		BaseURL:    baseUrl,
		HTTPClient: client,
	}

	res := Spec{}
	if err := cl.sendRequest(GET_REQUEST, V1_CONFIG_SPEC_ENDPOINT, &res); err != nil {
		return err
	}

	cl.Spec = res

	*cls = append(*cls, &cl)
	return nil
}
