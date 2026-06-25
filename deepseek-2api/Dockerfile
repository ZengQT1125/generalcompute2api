# 使用官方轻量级 Node.js 运行环境镜像
FROM node:20-alpine

# 设置工作目录
WORKDIR /app

# 复制依赖定义文件
COPY package*.json ./

# 安装生产环境依赖
RUN npm ci --only=production

# 复制核心代码和编译好的 WASM 模块
COPY src ./src
COPY sha3_wasm_bg.wasm ./

# 设置运行时环境变量默认值
ENV PORT=3000
ENV NODE_ENV=production
ENV LOG_DIR=/app/logs

# 声明容器暴露的端口
EXPOSE 3000

# 运行启动指令
CMD ["node", "src/index.js"]
