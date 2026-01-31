#!/usr/bin/env bash
#═══════════════════════════════════════════════════════════════════════════════
#                     Phantom-X 一键安装脚本
#═══════════════════════════════════════════════════════════════════════════════

set -e

GITHUB_REPO="yourname/phantom-x"
INSTALL_DIR="/opt/phantom-x"
CONFIG_DIR="/etc/phantom-x"
SERVICE_NAME="phantom-x"

RED='\033[0;31m'
GREEN='\033[0;32m'
YELLOW='\033[0;33m'
BLUE='\033[0;34m'
NC='\033[0m'

info()  { echo -e "${GREEN}[✓]${NC} $1"; }
warn()  { echo -e "${YELLOW}[!]${NC} $1"; }
error() { echo -e "${RED}[✗]${NC} $1"; exit 1; }
step()  { echo -e "${BLUE}[→]${NC} $1"; }

check_root() {
    [[ $EUID -ne 0 ]] && error "请使用 root 权限运行"
}

detect_arch() {
    case "$(uname -m)" in
        x86_64|amd64)   ARCH="amd64" ;;
        aarch64|arm64)  ARCH="arm64" ;;
        armv7l|armv7)   ARCH="arm" ;;
        *)              error "不支持的架构: $(uname -m)" ;;
    esac
}

detect_os() {
    case "$(uname -s)" in
        Linux)  OS="linux" ;;
        Darwin) OS="darwin" ;;
        *)      error "不支持的系统: $(uname -s)" ;;
    esac
}

get_latest_version() {
    curl -s "https://api.github.com/repos/${GITHUB_REPO}/releases/latest" | \
        grep '"tag_name"' | head -1 | sed -E 's/.*"v?([^"]+)".*/\1/'
}

install_server() {
    check_root
    detect_arch
    detect_os

    step "安装 Phantom-X 服务端..."

    VERSION=$(get_latest_version)
    if [[ -z "$VERSION" ]]; then
        error "无法获取版本信息"
    fi
    info "版本: v${VERSION}"

    mkdir -p "$INSTALL_DIR" "$CONFIG_DIR"

    DOWNLOAD_URL="https://github.com/${GITHUB_REPO}/releases/download/v${VERSION}/phantom-x-server-${OS}-${ARCH}.tar.gz"
    
    step "下载中..."
    curl -fsSL -o /tmp/phantom-x-server.tar.gz "$DOWNLOAD_URL" || error "下载失败"
    
    step "解压安装..."
    tar -xzf /tmp/phantom-x-server.tar.gz -C "$INSTALL_DIR"
    chmod +x "${INSTALL_DIR}/phantom-x-server"
    
    # 创建配置文件
    if [[ ! -f "${CONFIG_DIR}/server.yaml" ]]; then
        cat > "${CONFIG_DIR}/server.yaml" << 'EOF'
listen: ":443"
cert: "/etc/phantom-x/cert.pem"
key: "/etc/phantom-x/key.pem"
token: ""
ws_path: "/ws"
log_level: "info"
EOF
        info "配置文件已创建: ${CONFIG_DIR}/server.yaml"
    fi

    # 创建 systemd 服务
    cat > "/etc/systemd/system/${SERVICE_NAME}-server.service" << EOF
[Unit]
Description=Phantom-X Server
After=network.target

[Service]
Type=simple
ExecStart=${INSTALL_DIR}/phantom-x-server -c ${CONFIG_DIR}/server.yaml
Restart=always
RestartSec=3
LimitNOFILE=1048576

[Install]
WantedBy=multi-user.target
EOF

    systemctl daemon-reload
    systemctl enable "${SERVICE_NAME}-server"

    info "安装完成！"
    echo ""
    echo "下一步操作:"
    echo "  1. 配置 TLS 证书: ${CONFIG_DIR}/cert.pem 和 ${CONFIG_DIR}/key.pem"
    echo "  2. 编辑配置: ${CONFIG_DIR}/server.yaml"
    echo "  3. 启动服务: systemctl start ${SERVICE_NAME}-server"
    echo "  4. 查看日志: journalctl -u ${SERVICE_NAME}-server -f"
}

install_client() {
    detect_arch
    detect_os

    step "安装 Phantom-X 客户端..."

    VERSION=$(get_latest_version)
    [[ -z "$VERSION" ]] && error "无法获取版本信息"
    info "版本: v${VERSION}"

    INSTALL_PATH="/usr/local/bin/phantom-x"
    DOWNLOAD_URL="https://github.com/${GITHUB_REPO}/releases/download/v${VERSION}/phantom-x-client-${OS}-${ARCH}.tar.gz"

    step "下载中..."
    curl -fsSL -o /tmp/phantom-x-client.tar.gz "$DOWNLOAD_URL" || error "下载失败"

    step "安装..."
    tar -xzf /tmp/phantom-x-client.tar.gz -C /tmp
    
    if [[ -w "/usr/local/bin" ]]; then
        mv /tmp/phantom-x-client "$INSTALL_PATH"
    else
        sudo mv /tmp/phantom-x-client "$INSTALL_PATH"
    fi
    chmod +x "$INSTALL_PATH"

    rm -f /tmp/phantom-x-client.tar.gz

    info "安装完成！"
    echo ""
    echo "使用方法:"
    echo "  phantom-x -s wss://server:443/ws -token your-token"
}

uninstall() {
    check_root

    step "卸载 Phantom-X..."

    systemctl stop "${SERVICE_NAME}-server" 2>/dev/null || true
    systemctl disable "${SERVICE_NAME}-server" 2>/dev/null || true

    rm -f "/etc/systemd/system/${SERVICE_NAME}-server.service"
    rm -rf "$INSTALL_DIR"
    rm -f "/usr/local/bin/phantom-x"

    systemctl daemon-reload

    echo ""
    read -rp "是否删除配置文件? [y/N]: " del_config
    if [[ "$del_config" =~ ^[Yy]$ ]]; then
        rm -rf "$CONFIG_DIR"
        info "配置文件已删除"
    fi

    info "卸载完成"
}

show_help() {
    cat << EOF
Phantom-X 安装脚本

用法: $0 [命令]

命令:
  server    安装服务端
  client    安装客户端
  uninstall 卸载
  help      显示帮助

示例:
  $0 server   # 安装服务端
  $0 client   # 安装客户端
EOF
}

case "${1:-help}" in
    server)    install_server ;;
    client)    install_client ;;
    uninstall) uninstall ;;
    *)         show_help ;;
esac
