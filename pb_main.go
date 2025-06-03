package main

import (
	"log"

	"github.com/pocketbase/pocketbase"
	"github.com/pocketbase/pocketbase/core"
	"github.com/pocketbase/pocketbase/tools/hook"
)

func main() {
	app := pocketbase.New()

	app.OnServe().Bind(&hook.Handler[*core.ServeEvent]{
		Func: func(e *core.ServeEvent) error {
			group := e.Router.Group("/yt")
			registerPbRoutes(e.App, group)
			return e.Next()
		},
	})

	if err := app.Start(); err != nil {
		log.Fatal(err)
	}
}
