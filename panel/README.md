# workbuddy2api 管理台

管理台与网关组成一个运行版本。Go 主程序通过 `assets.go` 内嵌 Python 后端和页面，启动私有 Unix socket 子进程并提供 `/admin/` 路由。默认只有 `workbuddy2api` 一个容器、宿主回环 `7863` 一个端口，不需要 `docker.sock` 或独立面板镜像。

后端只使用 Python 标准库；前端由 `src/`、`app.js`、`keys.js`、`usage.js`、`requests.js` 等生成 `index.html`。`native_runtime.py` 提供受限的本地登录、积分、日志和重载适配，调用当前运行包中的辅助程序。

## 使用

按根目录 [新部署说明](../README.md#新部署) 准备五个持久化目录后启动：

```bash
docker compose up -d --build
```

访问 `http://127.0.0.1:7863/admin/`。存量 `panel-data/credentials.json`、会话撤销记录和密码信息保留；新部署的随机初始密码写入 `panel-data/initial-password.txt`。页面中的“重启服务”使用同版本重载，不再调用宿主 Docker。

账号、密钥复制、统计口径、HTTPS 代理和历史两容器迁移见 [管理台部署](../docs/panel.md)。

v2.4.0 可在密钥行设置频率、并发与等待，并跳转查看该调用方的请求消费和调度原因。未知用量、读取失败和留存范围均显式展示。操作方法见 [限流与请求明细](../docs/operations-observability.md)。

## 开发与测试

```bash
python3 panel/build.py
git diff --exit-code -- panel/index.html
python3 -m unittest discover -s panel -p 'test_*.py' -v
go test ./internal/panelruntime ./internal/server ./internal/hotupdate
```

修改页面源文件后生成并提交 `index.html`，再编译网关；内嵌前后端随主程序一起更新。Node.js 仅用于前端行为回归，不是面板运行依赖。CI 使用 Python 3.12。

| 文件 | 作用 |
|---|---|
| `assets.go` | 固定内嵌清单，排除运行凭据和测试数据 |
| `app.py`、`file_lock.py` | 页面、会话认证、跨进程状态保护 |
| `native_runtime.py` | 统一运行版受限本地操作 |
| `key_management.py` | 经私有管理 socket 访问密钥、用量和更新 |
| `src/`、`app.js`、`keys.js`、`usage.js`、`requests.js` | 页面源文件与各功能行为 |
| `build.py`、`index.html` | 页面生成器及产物 |

旧 Docker 适配仅用于历史部署和回滚验证，不是新的部署主线。
