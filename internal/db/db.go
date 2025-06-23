package db

import (
	"fmt"
	"log"
	"os"
	"time"

	"gorm.io/gorm/logger"

	"gorm.io/driver/postgres"
	"gorm.io/gorm"
)

func Connect() (*gorm.DB, error) {
	host := getenv("DB_HOST", "localhost")
	port := getenv("DB_PORT", "5432")
	user := getenv("DB_USER", "bitcoin")
	password := getenv("DB_PASSWORD", "bitcoin")
	dbname := getenv("DB_NAME", "bitcoin")
	dsn := fmt.Sprintf("host=%s port=%s user=%s password=%s dbname=%s sslmode=disable", host, port, user, password, dbname)

	// Create custom logger with reduced verbosity
	newLogger := logger.New(
		log.New(os.Stdout, "\r\n", log.LstdFlags),
		logger.Config{
			SlowThreshold:             time.Second,
			LogLevel:                  logger.Error, // Only show errors
			IgnoreRecordNotFoundError: true,
			Colorful:                  false,
		},
	)

	return gorm.Open(postgres.Open(dsn), &gorm.Config{
		Logger: newLogger,
	})
}

func getenv(key, def string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return def
}
