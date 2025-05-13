# Base path used to install.
CMD_DESTDIR ?= /usr/local
GO111MODULE_VALUE=auto
PREFIX ?= $(CURDIR)/out/
CMD_BINARIES=$(addprefix $(PREFIX),$(CMD))

CMD=ctr-erofs

all: build

build: $(CMD)

FORCE:

ctr-erofs: FORCE
	cd cmd/ ; GO111MODULE=$(GO111MODULE_VALUE) go build -o $(PREFIX)$@ $(GO_BUILD_FLAGS) $(GO_LD_FLAGS) -v ./ctr-erofs

install:
	@echo "$@"
	@mkdir -p $(CMD_DESTDIR)/bin
	@install $(CMD_BINARIES) $(CMD_DESTDIR)/bin

uninstall:
	@echo "$@"
	@rm -f $(addprefix $(CMD_DESTDIR)/bin/,$(notdir $(CMD_BINARIES)))

clean:
	@echo "$@"
	@rm -f $(CMD_BINARIES)
