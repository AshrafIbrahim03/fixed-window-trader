package main

import (
	"errors"
	"fmt"
	"os"
	"sort"
	"time"

	"github.com/alpacahq/alpaca-trade-api-go/v3/alpaca"
	"github.com/alpacahq/alpaca-trade-api-go/v3/marketdata"
	"github.com/joho/godotenv"
	"github.com/shopspring/decimal"
)

type (
	DataClient interface {
		// gets data from the current time minus the time window to the current time
		GetData(time_window uint64) []Dollar
	}
	TradeClient interface {
		//Makes a trade based on current data
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
	//runningTotal := float64(0)

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
	//fmt.Println("this array should be in ascending order", trades)

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
	dataClient  *RunningMeanDataClient
	tradeClient *alpaca.Client
}

func NewFixedWindowTrader(symbol string, slopeTarget SlopeTarget, dataClient *RunningMeanDataClient, tradeClient *alpaca.Client) (*FixedWindowTrader, error) {

	if slopeTarget.timeScale != dataClient.timeWindow {
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

func (fwt *FixedWindowTrader) Trade() (*alpaca.Order, error) {
	is_market_open, m_err := fwt.IsMarketOpen()
	if m_err != nil {
		return nil, m_err
	}
	if !is_market_open {
		return nil, errors.New("Not a valid time to trade or network is down")
	}

	recent_trades, t_err := fwt.dataClient.GetTrades(fwt.symbol)
	if t_err != nil {
		return nil, t_err
	}

	if len(recent_trades) == 0 {
		return nil, errors.New("No trades are received")
	}
	current_slope := (recent_trades[len(recent_trades)-1] - recent_trades[0]) / Dollar(fwt.dataClient.timeWindow.Seconds())
	fmt.Println("current slope is", current_slope)
	acc, a_err := fwt.tradeClient.GetAccount()
	if a_err != nil {
		return nil, a_err
	}

	if current_slope >= Dollar(fwt.slopeTarget.target) && acc.BuyingPower.GreaterThanOrEqual(decimal.Zero) {
		//buy stonks if possible
		if acc.BuyingPower.LessThanOrEqual(decimal.NewFromInt(1)) {
			return nil, nil
			fmt.Println("Not enough buying power to place an order on", fwt.symbol)
		}
		order, o_err := fwt.tradeClient.PlaceOrder(alpaca.PlaceOrderRequest{
			Symbol: fwt.symbol, Notional: &acc.BuyingPower, Type: "market", Side: "buy", TimeInForce: alpaca.TimeInForce("day"),
		})
		if o_err != nil {
			return nil, o_err
		}

		return order, nil
	}
	//sell stonks
	position, p_err := fwt.tradeClient.GetPosition(fwt.symbol)
	if p_err != nil {
		// If error is 404 (position does not exist), it's not really an error for us
		if apiErr, ok := p_err.(*alpaca.APIError); ok && apiErr.StatusCode == 404 {
			fmt.Println("No position exists to sell — skipping sell.")
			return nil, nil
		}
		return nil, p_err // actual error
	}
	//////	if position.QtyAvailable.GreaterThan(decimal.Zero) {
	//return nil, nil
	//}
	order, o_err := fwt.tradeClient.PlaceOrder(alpaca.PlaceOrderRequest{
		Symbol: fwt.symbol, Qty: &position.QtyAvailable, Type: "market", Side: "sell", TimeInForce: alpaca.TimeInForce("day"),
	})
	if o_err != nil {
		return nil, o_err
	}
	return order, nil
}

func main() {
	env_err := godotenv.Load()
	if env_err != nil {
	}
	apca_key := os.Getenv("APCA_API_KEY_ID")
	apca_secret := os.Getenv("APCA_API_SECRET_KEY")
	base_url := os.Getenv("APCA_API_BASE_URL")
	//fmt.Println("apca_key:", apca_key, "apca_secret:", apca_secret) //, "base_url:", base_url)
	TIME_WINDOW := time.Minute
	NUM_TIMES_TRADE_PER_WINDOW := 20.0
	time_between_trades := TIME_WINDOW.Seconds() / NUM_TIMES_TRADE_PER_WINDOW
	time_scale := time.Second

	data_client := marketdata.NewClient(marketdata.ClientOpts{
		APIKey:    apca_key,
		APISecret: apca_secret,
		//BaseURL:   base_url,
		Feed: marketdata.IEX,
	})
	trade_client := alpaca.NewClient(alpaca.ClientOpts{
		APIKey:    apca_key,
		APISecret: apca_secret,
		BaseURL:   base_url,
	})
	rmdc := RunningMeanDataClient{dataClient: data_client, meanPeriods: 10 * time.Minute, timeWindow: TIME_WINDOW}

	//trades, rmdc_err := rmdc.GetTrades("AAPL")
	//if rmdc_err != nil {
	//		fmt.Println("There's an error:")
	//		fmt.Println(rmdc_err)
	//	}
	//	fmt.Println(trades)

	fwt, fwt_err := NewFixedWindowTrader("TSLA", SlopeTarget{target: 0, timeScale: TIME_WINDOW}, &rmdc, trade_client)
	if fwt_err != nil {
		fmt.Println(fwt_err)
	}
	for {
		time_to_sleep := time.Duration(time_between_trades) * time_scale
		fmt.Println("Sleeping for", time_to_sleep.Seconds(), "seconds")
		time.Sleep(time_to_sleep)
		order, order_err := fwt.Trade()
		if order_err != nil {
			fmt.Println("order error:", order_err)
		}
		if order != nil {
			fmt.Println(time.Now(), order.Side, order.FilledQty, "orders of ", order.Symbol, "at a price of", order.Notional)
		} else {
			fmt.Println(time.Now(), "No trade made")
		}

		fmt.Println()
	}

}
