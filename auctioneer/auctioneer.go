package auctioneer

import (
	"os"
	"syscall"
	"time"

	"github.com/cloudfoundry-incubator/auction/auctionrunner"
	"github.com/cloudfoundry-incubator/auction/auctiontypes"
	"github.com/pivotal-golang/lager"

	Bbs "github.com/cloudfoundry-incubator/runtime-schema/bbs"

	"github.com/cloudfoundry-incubator/runtime-schema/metric"
	"github.com/cloudfoundry-incubator/runtime-schema/models"
)

const (
	startAuctionsStarted = metric.Counter("AuctioneerStartAuctionsStarted")
	startAuctionsFailed  = metric.Counter("AuctioneerStartAuctionsFailed")
	stopAuctionsStarted  = metric.Counter("AuctioneerStopAuctionsStarted")
	stopAuctionsFailed   = metric.Counter("AuctioneerStopAuctionsFailed")
)

type Auctioneer struct {
	bbs           Bbs.AuctioneerBBS
	runner        auctiontypes.AuctionRunner
	maxConcurrent int
	maxRounds     int
	logger        lager.Logger
	semaphore     chan bool
	lockInterval  time.Duration
}

func New(bbs Bbs.AuctioneerBBS, runner auctiontypes.AuctionRunner, maxConcurrent int, maxRounds int, lockInterval time.Duration, logger lager.Logger) *Auctioneer {
	return &Auctioneer{
		bbs:           bbs,
		runner:        runner,
		maxConcurrent: maxConcurrent,
		maxRounds:     maxRounds,
		logger:        logger.Session("auctioneer"),
		semaphore:     make(chan bool, maxConcurrent),
		lockInterval:  lockInterval,
	}
}

func (a *Auctioneer) Run(signals <-chan os.Signal, ready chan<- struct{}) error {
	var startAuctionChan <-chan models.LRPStartAuction
	var startErrorChan <-chan error
	var cancelStartWatchChan chan<- bool

	var stopAuctionChan <-chan models.LRPStopAuction
	var stopErrorChan <-chan error
	var cancelStopWatchChan chan<- bool

	for {
		if startAuctionChan == nil {
			startAuctionChan, cancelStartWatchChan, startErrorChan = a.bbs.WatchForLRPStartAuction()
			a.logger.Info("watching-for-start-auctions")
		}

		if stopAuctionChan == nil {
			stopAuctionChan, cancelStopWatchChan, stopErrorChan = a.bbs.WatchForLRPStopAuction()
			a.logger.Info("watching-for-stop-auctions")
		}

		if ready != nil {
			close(ready)
			ready = nil
		}

		select {
		case startAuction, ok := <-startAuctionChan:
			if !ok {
				startAuctionChan = nil
				continue
			}

			logger := a.logger.Session("start", lager.Data{
				"start-auction": startAuction,
			})
			go a.runStartAuction(startAuction, logger)

		case stopAuction, ok := <-stopAuctionChan:
			if !ok {
				stopAuctionChan = nil
				continue
			}

			logger := a.logger.Session("stop", lager.Data{
				"stop-auction": stopAuction,
			})
			go a.runStopAuction(stopAuction, logger)

		case err := <-startErrorChan:
			a.logger.Error("watching-start-auctions-failed", err)
			startAuctionChan = nil
			startErrorChan = nil

		case err := <-stopErrorChan:
			a.logger.Error("watching-stop-auctions-failed", err)
			stopAuctionChan = nil
			stopErrorChan = nil

		case sig := <-signals:
			if a.shouldStop(sig) {
				if cancelStartWatchChan != nil {
					a.logger.Info("stopping-start-watch")
					close(cancelStartWatchChan)
				}

				if cancelStopWatchChan != nil {
					a.logger.Info("stopping-stop-watch")
					close(cancelStopWatchChan)
				}

				return nil
			}
		}
	}

	return nil
}

func (a *Auctioneer) shouldStop(sig os.Signal) bool {
	return sig == syscall.SIGINT || sig == syscall.SIGTERM
}

func (a *Auctioneer) runStartAuction(startAuction models.LRPStartAuction, logger lager.Logger) {
	a.semaphore <- true
	defer func() {
		<-a.semaphore
	}()

	logger.Info("received")

	//claim
	err := a.bbs.ClaimLRPStartAuction(startAuction)
	if err != nil {
		logger.Debug("failed-to-claim", lager.Data{"error": err.Error()})
		return
	}

	defer a.bbs.ResolveLRPStartAuction(startAuction)

	cellGuids, err := a.getCellsforStack(startAuction.DesiredLRP.Stack)
	if err != nil {
		logger.Error("failed-to-get-cells", err)
		return
	}
	if len(cellGuids) == 0 {
		logger.Error("no-available-cells", nil)
		return
	}

	//perform auction
	logger.Info("performing")
	startAuctionsStarted.Increment()

	rules := auctionrunner.DefaultStartAuctionRules
	rules.MaxRounds = a.maxRounds

	request := auctiontypes.StartAuctionRequest{
		LRPStartAuction: startAuction,
		RepGuids:        cellGuids,
		Rules:           rules,
	}

	_, err = a.runner.RunLRPStartAuction(request)
	if err != nil {
		logger.Error("auction-failed", err)
		startAuctionsFailed.Increment()
		return
	}
}

func (a *Auctioneer) getCellsforStack(stack string) ([]string, error) {
	cells, err := a.bbs.GetAllCells()
	if err != nil {
		return nil, err
	}

	filteredCellGuids := []string{}

	for _, cell := range cells {
		if cell.Stack == stack {
			filteredCellGuids = append(filteredCellGuids, cell.CellID)
		}
	}

	return filteredCellGuids, nil
}

func (a *Auctioneer) runStopAuction(stopAuction models.LRPStopAuction, logger lager.Logger) {
	logger.Debug("received")

	//claim
	err := a.bbs.ClaimLRPStopAuction(stopAuction)
	if err != nil {
		logger.Debug("failed-to-claim", lager.Data{"error": err.Error()})
		return
	}

	defer a.bbs.ResolveLRPStopAuction(stopAuction)

	cellGuids, err := a.getCells()
	if err != nil {
		logger.Error("failed-to-get-cells", err)
		return
	}

	if len(cellGuids) == 0 {
		logger.Error("no-available-cells", nil)
		return
	}

	//perform auction
	logger.Info("perform")
	stopAuctionsStarted.Increment()

	request := auctiontypes.StopAuctionRequest{
		LRPStopAuction: stopAuction,
		RepGuids:       cellGuids,
	}
	_, err = a.runner.RunLRPStopAuction(request)

	if err != nil {
		logger.Error("auction-failed", err)
		stopAuctionsFailed.Increment()
		return
	}
}

func (a *Auctioneer) getCells() ([]string, error) {
	cells, err := a.bbs.GetAllCells()
	if err != nil {
		return nil, err
	}

	cellGuids := []string{}

	for _, cell := range cells {
		cellGuids = append(cellGuids, cell.CellID)
	}

	return cellGuids, nil
}
