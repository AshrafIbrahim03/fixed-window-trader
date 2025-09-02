package main

import (
	"context"
	"errors"
	"fmt"
	"math/rand/v2"
	"os"
	"sort"
	"time"

	"github.com/alpacahq/alpaca-trade-api-go/v3/alpaca"
	"github.com/alpacahq/alpaca-trade-api-go/v3/marketdata"
	"github.com/joho/godotenv"
	"github.com/shopspring/decimal"
	"golang.org/x/time/rate"
)

type (
	DataClient interface {
		// gets data from the current time minus the time window to the current time
		GetTrades(ticket string) ([]Dollar, error)
		// gets the window that the data is on. It starts at the current time minus the time window
		// to the current time
		GetTimeWindow() time.Duration
	}
	TradeClient interface {
		// Makes a trade based on current data
		Trade() error

		// Returns whether the market is open or not
		IsMarketOpen(bool, error)
	}
	Dollar float64
)

type RunningMeanDataClient struct {
	dataClient  *marketdata.Client
	timeWindow  time.Duration // the total amount of time the means are calculated over
	meanPeriods time.Duration // the time each average is done over
}

func (rmdc *RunningMeanDataClient) GetTimeWindow() time.Duration {
	return rmdc.timeWindow
}

// Returns the trades from the specified time_window
// time_window is the amount of time from the present the window goes into the past
// ticker is the string ticker to be getting info for
func (rmdc *RunningMeanDataClient) GetTrades(ticker string) ([]Dollar, error) {
	trades, err := rmdc.dataClient.GetTrades(ticker, marketdata.GetTradesRequest{Start: time.Now().Add(-rmdc.timeWindow), End: time.Now()})
	if err != nil {
		return nil, err
	}

	sort.Slice(trades, func(i, j int) bool {
		return trades[i].Timestamp.Before(trades[j].Timestamp)
	})

	runningMeans := make([]Dollar, 0)
	// runningTotal := float64(0)

	for i := range trades {
		windowStart := trades[i].Timestamp
		windowEnd := windowStart.Add(rmdc.meanPeriods)

		var sum float64
		var count int

		for j := i; j < len(trades); j++ {
			if trades[j].Timestamp.After(windowEnd) {
				break
			}
			sum += trades[j].Price
			count++
		}

		if count > 0 {
			runningMeans = append(runningMeans, Dollar(sum/float64(count)))
		}
	}
	// fmt.Println("this array should be in ascending order", trades)

	return runningMeans, nil
}

type SlopeTarget struct {
	target    float64
	timeScale time.Duration
}

// Trades if the slope is above the amount indicated in slope_target
type FixedWindowTrader struct {
	symbol      string
	slopeTarget SlopeTarget
	dataClient  DataClient
	tradeClient *alpaca.Client
}

func NewFixedWindowTrader(symbol string, slopeTarget SlopeTarget, dataClient DataClient, tradeClient *alpaca.Client) (*FixedWindowTrader, error) {
	if slopeTarget.timeScale != dataClient.GetTimeWindow() {
		// Figure out how to use any slope across any time scale
		errors.New("timescale and timeWindow should be the same. I need to figure out a solution later")
	}

	return &FixedWindowTrader{
		symbol:      symbol,
		slopeTarget: slopeTarget,
		dataClient:  dataClient,
		tradeClient: tradeClient,
	}, nil
}

func (fwt *FixedWindowTrader) IsMarketOpen() (bool, error) {
	clock, err := fwt.tradeClient.GetClock()
	if err != nil {
		return false, errors.New(fmt.Sprint("Could not get clock, error: ", err))
	}
	return clock.IsOpen, nil
}

func (fwt *FixedWindowTrader) waitForMarket() error {
	clock, err := fwt.tradeClient.GetClock()
	if err != nil {
		return errors.New(fmt.Sprint("Could not get clock, error: ", err))
	}
	timeUntilOpen := time.Until(clock.NextOpen)
	fmt.Println("Sleeping for", timeUntilOpen.Hours(), "hours until the market opens at", clock.NextOpen)

	time.Sleep(timeUntilOpen)
	return nil
}

func (fwt *FixedWindowTrader) Trade() (*alpaca.Order, error) {
	rateLimitErr := dataRateLimiter.Wait(context.Background())
	if rateLimitErr != nil {
		return nil, rateLimitErr
	}
	is_market_open, m_err := fwt.IsMarketOpen()
	if m_err != nil {
		return nil, m_err
	}
	if !is_market_open {
		return nil, errors.New("Not a valid time to trade or network is down")
	}

	fmt.Println("Getting info for", fwt.symbol)
	recent_trades, t_err := fwt.dataClient.GetTrades(fwt.symbol)
	if t_err != nil {
		return nil, t_err
	}

	if len(recent_trades) == 0 {
		return nil, errors.New("No trades are received")
	}
	currentSlope := (recent_trades[len(recent_trades)-1] - recent_trades[0]) / Dollar(fwt.dataClient.GetTimeWindow().Seconds())
	fmt.Println("current slope for", fwt.symbol, "is", currentSlope)
	acc, accErr := fwt.tradeClient.GetAccount()
	if accErr != nil {
		return nil, accErr
	}

	// the amount allocated to each trader
	amountAvailable := acc.BuyingPower.DivRound(decimal.NewFromInt(int64(NumTraders)), 2)

	fmt.Println("Starting trade decision for", fwt.symbol, "with", amountAvailable)
	rateLimitErr = tradeRateLimiter.Wait(context.Background())
	if rateLimitErr != nil {
		return nil, rateLimitErr
	}
	if currentSlope >= Dollar(fwt.slopeTarget.target) && acc.BuyingPower.GreaterThanOrEqual(decimal.Zero) {
		// buy stonks if possible
		if amountAvailable.LessThanOrEqual(decimal.NewFromInt(1)) {
			fmt.Println("Not enough buying power to place an order on", fwt.symbol)
			return nil, nil
		}

		// Try 5 times to make the order
		for i := range 5 {
			fmt.Println("Try number", i, "for", fwt.symbol)
			order, orderErr := fwt.tradeClient.PlaceOrder(alpaca.PlaceOrderRequest{
				Symbol: fwt.symbol, Notional: &amountAvailable, Type: "market", Side: "buy", TimeInForce: alpaca.TimeInForce("day"),
			})
			if orderErr != nil && orderErr.(*alpaca.APIError).Code == 429 {
			} else if orderErr != nil && orderErr.(*alpaca.APIError).Code != 429 {
				return nil, orderErr
			} else if orderErr == nil {
				return order, nil
			}

		}

		return nil, errors.New("Could not make trade")
	}
	// sell stonks
	position, p_err := fwt.tradeClient.GetPosition(fwt.symbol)
	if p_err != nil {
		// If error is 404 (position does not exist), it's not really an error for us
		if apiErr, ok := p_err.(*alpaca.APIError); ok && apiErr.StatusCode == 404 {
			fmt.Println("No position exists to sell — skipping sell.")
			return nil, nil
		}
		return nil, p_err // actual error
	}
	order, o_err := fwt.tradeClient.PlaceOrder(alpaca.PlaceOrderRequest{
		Symbol: fwt.symbol, Qty: &position.QtyAvailable, Type: "market", Side: "sell", TimeInForce: alpaca.TimeInForce("day"),
	})
	if o_err != nil {
		return nil, o_err
	}
	return order, nil
}

func RunFixedWindowTrader(fwt *FixedWindowTrader) {
	for {

		order, orderErr := fwt.Trade()
		if orderErr != nil {
			fmt.Println("order error:", orderErr)
		}
		if order != nil {
			filledQty := order.FilledQty
			notional := order.Notional
			if notional != nil {
				filledAmount, _ := filledQty.Float64()
				priceAmount, _ := notional.Float64()
				fmt.Println(time.Now(), order.Side, filledAmount, "orders of ", order.Symbol, "at a price of", priceAmount)
			} else {
				filledQty := order.FilledQty
				filledAmount, _ := filledQty.Float64()
				fmt.Println(time.Now(), order.Side, filledAmount, "orders of ", order.Symbol, "at a price of", "Unkown")
			}

		} else {
			fmt.Println(time.Now(), "No trade made for", fwt.symbol)
		}

		marketWaitingErr := fwt.waitForMarket()
		if marketWaitingErr != nil {
			fmt.Println(marketWaitingErr)
		}
	}
}

var (
	exitChannel                = make(chan bool)
	TimeWindow                 = 7 * 24 * time.Hour
	NUM_TIMES_TRADE_PER_WINDOW = 4.0
	TICKERS                    = []string{"TSLA", "AAPL", "NVDA", "PLTR", "MSFT", "AMZN", "META", "GOOG", "AVGO", "WMT", "NFLX", "MA", "V", "LLY", "ORCL"}
	NumTraders                 = len(TICKERS)
	timeBetweenTrades          = TimeWindow.Seconds() / NUM_TIMES_TRADE_PER_WINDOW
	timeScale                  = time.Second
	RateLimit                  = 150
	dataRateLimiter            = rate.NewLimiter(rate.Every(time.Minute/time.Duration(RateLimit)), 1)
	tradeRateLimiter           = rate.NewLimiter(rate.Every(time.Minute/time.Duration(RateLimit)), 1)
)

func RandomSleep() {
	toSleep := (20 * rand.Float64()) + 10
	time.Sleep(time.Duration(toSleep) * time.Second)
}

func main() {
	envErr := godotenv.Load()
	if envErr != nil {
		fmt.Println("Could not load env vars")
		return
	}
	apcaKey := os.Getenv("APCA_API_KEY_ID")
	apcaSecret := os.Getenv("APCA_API_SECRET_KEY")
	baseURL := os.Getenv("APCA_API_BASE_URL")

	dataClient := marketdata.NewClient(marketdata.ClientOpts{
		APIKey:    apcaKey,
		APISecret: apcaSecret,
		// BaseURL:   base_url,
		Feed: marketdata.IEX,
	})
	tradeClient := alpaca.NewClient(alpaca.ClientOpts{
		APIKey:    apcaKey,
		APISecret: apcaSecret,
		BaseURL:   baseURL,
	})
	rmdc := RunningMeanDataClient{dataClient: dataClient, meanPeriods: 10 * time.Minute, timeWindow: TimeWindow}

	// trades, rmdc_err := rmdc.GetTrades("AAPL")
	// if rmdc_err != nil {
	//		fmt.Println("There's an error:")
	//		fmt.Println(rmdc_err)
	//	}
	//	fmt.Println(trades)
	fwts := make([]*FixedWindowTrader, NumTraders)

	// making traders
	for i, t := range TICKERS {
		fwt, fwtErr := NewFixedWindowTrader(t, SlopeTarget{target: 0, timeScale: TimeWindow}, &rmdc, tradeClient)
		if fwtErr != nil {
			fmt.Println("FixedWindowTrader instantiation error")
			fmt.Println(fwtErr)
		} else {
			fwts[i] = fwt
		}

	}

	for _, fwt := range fwts {
		go RunFixedWindowTrader(fwt)
		time.Sleep(5 * time.Second)
	}
	// waits until it gets a message from the exit channel
	<-exitChannel
}
