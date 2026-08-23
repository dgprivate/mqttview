package main

import (
	"context"
	"fmt"
	"os"
	"time"

	"github.com/dgprivate/mqttview/internal/mqttc"
)

func main() {
	mgr := mqttc.NewManager(nil)
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	spec := mqttc.ConnectionSpec{
		ID: "probe", Name: "probe", URL: os.Args[1], Version: mqttc.V5,
		CleanStart:    true,
		Subscriptions: []mqttc.Subscription{{Filter: "probe/#", QoS: 1}},
	}
	if _, err := mgr.Upsert(ctx, spec); err != nil {
		fmt.Println("upsert:", err)
		return
	}
	if err := mgr.Connect(ctx, "probe"); err != nil {
		fmt.Println("connect:", err)
		return
	}
	time.Sleep(4 * time.Second)
	st := mgr.List()[0].Status()
	fmt.Println("state:", st.State, "lastError:", st.LastError)
	mgr.Shutdown(context.Background())
}
