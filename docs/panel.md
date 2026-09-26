# 管理台部署与迁移

统一运行版在同一个 Go 入口提供 `/v1/` 和 `/admin/`。管理台是该版本内嵌的 Python 子进程，只监听私有 Unix socket；账号、密钥、任务、日志和版本操作复用本地管理通道。

## 单容器布局

| 项目 | 统一运行版 |
|---|---|
| Compose 服务 / 容器 | `wb2api` / `workbuddy2api` |
| 宿主端口 | `127.0.0.1:7863` |
| 管理页面 | `http://127.0.0.1:7863/admin/` |
| 运行身份 | UID/GID `10001:10001`，移除额外 Linux capabilities |
| 配置挂载 | `./config:/app/config`，配置为 `config/config.json` |
| 保留的数据 | `auths/`、`auths-trash/`、`data/`、`panel-data/` |

不开放独立面板端口，也不挂载 Docker socket。五个目录需允许运行用户读写；已有文件改权限前保留备份。新部署命令见 [README](../README.md#新部署)。

## 管理员与操作

存量 `panel-data/credentials.json` 优先使用。没有凭据时生成 `wbadmin` 和随机密码，写入 `panel-data/initial-password.txt`。有意提供旧 htpasswd 时可继承；新 Compose 不依赖宿主 nginx 文件。损坏凭据保留并明确报错，不通过覆盖或自动换密掩盖问题。

改密会轮换会话密钥、撤销旧设备登录；退出持久化撤销当前会话。会话有效期 30 天，剩余不足 10 天时滑动续期。备份整个 `panel-data/`，不要只复制密码文件。

| 操作 | 页面行为 |
|---|---|
| 添加、启停、回收账号 | 调用同版登录工具，修改后重载账号配置 |
| 创建、复制、停用密钥 | 经私有管理 socket 操作，列表不返回完整值 |
| 模型绑定、到期时间 | 完整模型 id 匹配，过期明确返回 `api_key_expired` |
| 重复推理保护 | 每密钥选择启停；系统页选择命中后重发或停止 |
| 任务、日志、用量 | 查看任务与请求、读取本地滚动日志和真实用量账本 |
| 重启服务 / 版本更新 | 同版本重载或完整包更新，候选就绪后再交接 |

## 反向代理

HTTPS 站点可将路径原样转给 `7863`。以下片段放在已配置证书的 nginx `server` 内：

```nginx
client_max_body_size 8m;
location / {
    proxy_pass http://127.0.0.1:7863;
    proxy_http_version 1.1;
    proxy_set_header Host $http_host;
    proxy_set_header X-Real-IP $remote_addr;
    proxy_set_header X-Forwarded-For $remote_addr;
    proxy_set_header X-Forwarded-Proto $scheme;
    proxy_buffering off;
    proxy_request_buffering off;
    proxy_read_timeout 900s;
    proxy_send_timeout 900s;
}
```

只替换旧 `location /admin/` 时也使用 `proxy_pass http://127.0.0.1:7863;`，保留 `/admin/`。旧指向 `7864/`、剥掉前缀的代理不适用于统一入口。nginx 请求大小应与实际 `server.max_body_mb` 协调。

`WB2API_TRUSTED_PROXIES` 默认仅信任回环和默认 Docker bridge 的宿主地址 `172.17.0.1/32`。使用其他 bridge 或前置代理时填写实际直连代理地址。统一入口只采信可信对端的客户端 IP/HTTPS 信息，再重建发送给私有面板的头。前面有 CDN 时先在 nginx 配置真实来源信任规则。

登录限流按 IPv6 的 /64 分桶（同一网段轮换地址不能绕过），并在 1 分钟窗口内做全站失败节流：超过 20 次失败后短暂拒绝新的登录尝试，挡住轮换 IP 的分布式爆破，窗口很短、阈值远高于人工误输，不会把管理员锁在门外。

可选用 `WB2API_ADMIN_ALLOW_CIDRS` 限制管理入口来源，例如 `203.0.113.0/24,198.51.100.7/32`；留空（默认）不限制，回环地址始终放行，容器内就绪探针不受影响。

Cookie 使用 `HttpOnly`、`SameSite=Lax` 和根路径，可信 HTTPS 请求设置 `Secure`。管理 POST 需要已登录会话、JSON 对象和 `X-Admin-Request: 1`，Origin 必须同源。密钥和版本管理 socket 不应另行反向代理到公网。

## 更新签名

自更新只接受带发布者签名的发布：

- 发布流程用本机私钥对 `SHA256SUMS.txt` 做 ed25519 签名，产出 `SHA256SUMS.txt.sig` 一并上传；
- 网关二进制内置公钥（`gateway/release-signing.pub`，构建期注入），下载前验签、下载后与签名清单对账，签名缺失或不符直接拒绝，不提供「拿不到签名就放行」的降级；
- 私钥只留本机（默认 `~/.wb2api/release-signing.key`，0600，可用 `WB2A_RELEASE_KEY_FILE` 指定）；泄露或遗失用 `python tools/release_signing_20260926.py keygen --force` 轮换，但公钥写在二进制里，轮换后需要给所有部署重新分发一次由新密钥签名的发布（旧版会拒绝新密钥的签名）。

未注入公钥的开发构建会跳过验签并在日志里写明，仅用于本地调试。

## 日志与用量

日志同时写到标准输出和 `data/logs/gateway.log` 的有界轮转文件，页面不再依赖 `docker logs`。请求保留完整模型名、密钥归属、输入、缓存命中、输出、耗时和 `rid=`，可关联响应的 `X-Request-ID`。缺失用量显示未知；固定表头和横向滚动继续保留。

账本默认是密钥库同目录的 `usage.json`。每个实际发起上游调用的客户端请求记录一次；内部重试合计各次已知消费，不重复增加请求数。失败、取消和输出约束拒绝保留已知用量，缺失部分标成未完整上报。缓存属于输入、思考属于输出，不额外相加，也不乘固定倍率。

日期筛选只合计已有日期桶；模型拆分是累计维度。旧版本未记录的日期、失败或缺失用量无法从新字段反推。`include_usage:false` 只隐藏原生 Chat 的响应展示，不关闭内部记账。

## 从旧两容器部署迁移

首次切换需要维护窗口和完整镜像，不能只替换旧主程序。下面是操作顺序，不表示生产迁移已经完成：

1. 记录旧镜像、Compose、代理、实际版本及运行文件摘要。保留 `2.1.29` Release 和旧镜像作为回滚来源。
2. 等在途请求收尾，停止旧网关与旧面板，取得一致备份：原配置、五个持久化目录和 `data/updates/current`；密钥库与 `.enc-key` 必须一起保留。
3. 将旧根目录 `config.json` 复制到新 `config/config.json`，保留原设置，确认 `update.repo` 使用公开发布源 `dddmiku/workbuddy2api-maintenance`；已有同名设置无需修改，公开下载时 `update.token` 可留空。只对这些明确目录校准 `10001:10001` 权限，不重新初始化账号、密钥或管理员。
4. 用单服务 Compose 启动完整镜像。统一入口会忽略缺少 manifest 的旧裸二进制更新指针；仍需核对 `/healthz` 和实际进程文件，不能只看镜像标签。
5. 管理代理由 `7864` 改成 `7863` 并保留 `/admin/`。确认登录、账号、密钥复制、权限、日志、累计用量、任务和真实客户端请求，再停用旧面板发布端口。

回滚先停止新运行版，再用记录的旧镜像、旧 Compose 和对应配置恢复历史服务及代理；不要让旧、新部署同时写一份状态。恢复整份备份会回退备份后的新增记录；需要保留新数据时先核对格式兼容性。

日常运行包不替换持久化目录，候选完成校验和就绪握手后才提交 `current`，失败保留旧版本。常规停止、重载和更新共享 15 分钟收尾预算，Compose 提供 16 分钟停止宽限；超时仍可能中断。基础镜像、解释器和 PID 1 脚本变更需重建镜像。详见 [配置说明](configuration.md#热更新)。

## 排障

| 现象 | 检查 |
|---|---|
| `/admin/` 404 | 代理是否剥掉前缀，是否仍指向旧 `7864` |
| 管理台 502 | 唯一容器日志中的内嵌面板启动错误、Python 和目录权限 |
| 登录页正常但健康为 503 | 是否有可服务账号，当前并发额度是否占满 |
| 密钥复制失败 | 密钥库与 `.enc-key` 是否一起迁移；只含摘要的旧记录需先被原密钥成功使用 |
| 配置未生效 | 是否挂载整个 `config/`，两端是否读取同一配置，重载是否成功 |
| 用量偏少 | 未完整上报次数、日期范围和重试记录；历史未知消费不能精确补回 |
| 更新 404/403 | `update.repo`、对应 Release/运行包是否存在及 GitHub 访问限流；自选私有源时另查读取权限，调用 API key 不是下载凭据 |

源码分工与开发说明见 [panel/README.md](../panel/README.md)。
