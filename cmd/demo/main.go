// Command demo runs the four demo scenarios against a running stack and
// exits non-zero when any result is not the expected one.
package main

import (
	"bytes"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"net/http"
	"os"
	"strings"
	"time"
)

type checker struct {
	base     string
	client   *http.Client
	failures int
}

type obj = map[string]any

func (c *checker) call(method, path string, body any) (int, obj) {
	var buf bytes.Buffer
	if body != nil {
		_ = json.NewEncoder(&buf).Encode(body)
	}
	req, err := http.NewRequest(method, c.base+path, &buf)
	if err != nil {
		c.fatal(err)
	}
	req.Header.Set("Content-Type", "application/json")
	resp, err := c.client.Do(req)
	if err != nil {
		c.fatal(err)
	}
	defer resp.Body.Close()
	raw, _ := io.ReadAll(resp.Body)
	var out obj
	_ = json.Unmarshal(raw, &out)
	return resp.StatusCode, out
}

func (c *checker) fatal(err error) {
	fmt.Println("FAIL", err)
	os.Exit(1)
}

func (c *checker) check(name string, ok bool, detail ...any) {
	if ok {
		fmt.Println("  ok  ", name)
		return
	}
	c.failures++
	fmt.Println("  FAIL", name, fmt.Sprint(detail...))
}

func (c *checker) state() obj {
	status, st := c.call("GET", "/demo/state", nil)
	if status != http.StatusOK {
		c.fatal(fmt.Errorf("GET /demo/state answered %d", status))
	}
	return st
}

func (c *checker) order(mid string) obj {
	for _, o := range st(c.state(), "orders") {
		if o.(obj)["marketplaceOrderId"] == mid {
			return o.(obj)
		}
	}
	return nil
}

func st(o obj, key string) []any {
	list, _ := o[key].([]any)
	return list
}

func (c *checker) wait(what string, timeout time.Duration, cond func() bool) bool {
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		if cond() {
			return true
		}
		time.Sleep(300 * time.Millisecond)
	}
	c.check("waiting for "+what, false, "timed out after ", timeout)
	return false
}

func (c *checker) place(mid string) obj {
	status, got := c.call("POST", "/demo/orders", obj{"marketplaceOrderId": mid, "sku": "6", "quantity": 1})
	if status != http.StatusOK {
		c.fatal(fmt.Errorf("placing %s answered %d: %v", mid, status, got))
	}
	return got
}

func (c *checker) fulfill(mid string) {
	if status, got := c.call("POST", "/demo/orders/"+mid+"/fulfill", nil); status != http.StatusOK {
		c.fatal(fmt.Errorf("fulfilling %s answered %d: %v", mid, status, got))
	}
}

func stock(state obj, sku string) float64 {
	for _, s := range st(state, "stock") {
		if s.(obj)["sku"] == sku {
			return s.(obj)["available"].(float64)
		}
	}
	return -1
}

func (c *checker) countEvents(order obj, source string, level string) int {
	n := 0
	for _, e := range st(order, "events") {
		ev := e.(obj)
		if ev["source"] == source && ev["level"] == level {
			n++
		}
	}
	return n
}

func randomID(prefix string) string {
	b := make([]byte, 4)
	_, _ = rand.Read(b)
	return prefix + "-" + hex.EncodeToString(b)
}

func (c *checker) reset() {
	if status, _ := c.call("POST", "/demo/reset", nil); status != http.StatusOK {
		c.fatal(fmt.Errorf("reset answered %d", status))
	}
}

func main() {
	base := flag.String("base", "http://127.0.0.1:8094", "base URL of the running stack")
	flag.Parse()
	c := &checker{base: strings.TrimRight(*base, "/"), client: &http.Client{Timeout: 15 * time.Second}}

	fmt.Println("Preparation: reset")
	c.reset()
	c.check("state is clean after reset", len(st(c.state(), "orders")) == 0 && len(st(c.state(), "dlq")) == 0)

	fmt.Println("Scenario 1: duplicate order yields a single order")
	before := stock(c.state(), "6")
	dup := randomID("DUP")
	first, second := c.place(dup), c.place(dup)
	c.check("second placement returns the same seller order", second["sellerOrderId"] == first["sellerOrderId"], first, second)
	c.check("second placement is flagged as duplicate", second["duplicate"] == true)
	state := c.state()
	c.check("exactly one order exists", len(st(state, "orders")) == 1, len(st(state, "orders")))
	c.check("stock was reserved once", stock(state, "6") == before-1, stock(state, "6"), " vs ", before-1)

	fmt.Println("Scenario 2: ERP down, retries, breaker opens, message goes to the DLQ")
	if status, _ := c.call("POST", "/demo/erp", obj{"failing": true}); status != http.StatusOK {
		c.fatal(fmt.Errorf("switching the ERP answered %d", status))
	}
	down := randomID("DOWN")
	c.place(down)
	c.fulfill(down)
	if c.wait("the dead letter", 30*time.Second, func() bool { return len(st(c.state(), "dlq")) == 1 }) {
		state = c.state()
		letter := st(state, "dlq")[0].(obj)
		c.check("DLQ keeps the reason with the breaker", strings.Contains(fmt.Sprint(letter["reason"]), "circuit breaker is open"), letter["reason"])
		c.check("DLQ keeps the attempt count", letter["attempts"].(float64) >= 3, letter["attempts"])
		c.check("circuit breaker is open", state["breaker"].(obj)["state"] == "open", state["breaker"])
		c.check("timeline shows the failed ERP attempts", c.countEvents(c.order(down), "erp", "error") >= 3, c.order(down))
		c.check("order did not advance past authorized", c.order(down)["status"] == "authorized", c.order(down)["status"])
	}

	fmt.Println("Scenario 3: ERP back + DLQ replay completes the order")
	if status, _ := c.call("POST", "/demo/erp", obj{"failing": false}); status != http.StatusOK {
		c.fatal(fmt.Errorf("switching the ERP answered %d", status))
	}
	if c.wait("the breaker to allow a probe", 30*time.Second, func() bool { return c.state()["breaker"].(obj)["state"] != "open" }) {
		status, res := c.call("POST", "/demo/dlq/replay", nil)
		c.check("replay reprocessed one letter", status == http.StatusOK && res["replayed"] == float64(1), status, res)
		if c.wait("the order to ship", 20*time.Second, func() bool { return c.order(down)["status"] == "shipped" }) {
			state = c.state()
			c.check("DLQ is empty", len(st(state, "dlq")) == 0)
			c.check("breaker closed again", state["breaker"].(obj)["state"] == "closed", state["breaker"])
			c.check("the ERP accepted the order exactly once", c.countEvents(c.order(down), "erp", "ok") == 1, c.order(down))
		}
	}

	fmt.Println("Scenario 4: happy path up to the tracking code")
	happy := randomID("OK")
	c.place(happy)
	c.fulfill(happy)
	if c.wait("shipped with the invoice received", 20*time.Second, func() bool {
		o := c.order(happy)
		return o["status"] == "shipped" && c.countEvents(o, "marketplace", "ok") >= 2
	}) {
		o := c.order(happy)
		c.check("tracking code came from the TMS", strings.HasPrefix(fmt.Sprint(o["trackingCode"]), "TRK-"), o["trackingCode"])
		c.check("marketplace received the invoice", strings.Contains(fmt.Sprint(o["events"]), "marketplace recebeu a nota"))
	}

	fmt.Println("Rate limit: a burst from one IP gets 429")
	got429 := false
	for i := 0; i < 80 && !got429; i++ {
		status, _ := c.call("GET", "/demo/state", nil)
		got429 = status == http.StatusTooManyRequests
	}
	c.check("429 after exceeding the budget", got429)
	time.Sleep(4 * time.Second)

	fmt.Println("Cleanup: reset")
	c.reset()
	c.check("state is clean after the final reset", len(st(c.state(), "orders")) == 0)

	if c.failures > 0 {
		fmt.Printf("\n%d check(s) failed\n", c.failures)
		os.Exit(1)
	}
	fmt.Println("\nall scenarios passed")
}
