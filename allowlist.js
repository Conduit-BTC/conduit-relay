#!/usr/bin/env node

const whiteList = {
  "f6485aa30cd334d3a4b749698d0e1e3f719657b3396ade1313525b4a305d8cab": true,
};

const rl = require("readline").createInterface({
  input: process.stdin,
  output: process.stdout,
  terminal: false,
});

rl.on("line", (line) => {
  let req = JSON.parse(line);

  if (req.type !== "new") {
    console.error("unexpected request type");
    return;
  }

  let res = { id: req.event.id }; // must echo the event's id

  if (whiteList[req.event.pubkey]) {
    res.action = "accept";
  } else {
    res.action = "reject";
    res.msg = "blocked: not on white-list";
  }

  console.log(JSON.stringify(res));
});
