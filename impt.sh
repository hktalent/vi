# go build -trimpath -o ./bin/vi -ldflags "-s -w" main.go
vi reset -data ./data -yes
./bin/vi importfaces -dir "$1" -label "$2"

