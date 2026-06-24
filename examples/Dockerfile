# Minimal placeholder so the `api` service in examples/compose.yaml has a
# build context. Replace with your real app image.
FROM node:22-alpine
WORKDIR /app
CMD ["node", "-e", "require('http').createServer((_,r)=>r.end('ok')).listen(3000)"]
