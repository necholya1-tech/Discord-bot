build:
	docker compose build

up:
	docker compose up -d

logs:
	docker compose logs -f bot

stop:
	docker compose down
