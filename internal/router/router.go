package router

import (
	"github.com/labstack/echo/v4"

	"github.com/karthikadarvin/wallet-transfer-assignment/internal/handler"
)

func Register(e *echo.Echo, transferHandler *handler.TransferHandler) {
	e.GET("/health", func(c echo.Context) error {
		return c.JSON(200, map[string]string{"status": "ok"})
	})

	e.POST("/transfers", transferHandler.Create)
}
