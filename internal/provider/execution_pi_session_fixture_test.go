package provider

import (
	"os"
	"os/exec"
	"path/filepath"
	"testing"
)

// This compiled child implements only an offline protocol fixture. It is not Pi,
// does not call a model/server, and cannot establish native product acceptance.
func piSessionBuildFixture(t *testing.T) string {
	t.Helper()
	dir := piSessionPrivateTemp(t)
	source, binary := filepath.Join(dir, "main.go"), filepath.Join(dir, "pi-fixture")
	if err := os.WriteFile(source, []byte(piSessionFixtureSource), 0600); err != nil {
		t.Fatal(err)
	}
	cmd := exec.Command("go", "build", "-p", "1", "-o", binary, source)
	if output, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("compile offline child fixture: %v\n%s", err, output)
	}
	return binary
}

const piSessionFixtureSource = `package main

import (
 "bytes"
 "crypto/rand"
 "crypto/sha256"
 "encoding/hex"
 "encoding/json"
 "fmt"
 "io"
 "os"
 "os/exec"
 "path/filepath"
 "strings"
 "time"
)

func must(err error) { if err != nil { panic("offline fixture failure") } }
func encode(v any) []byte { data, err := json.Marshal(v); must(err); return append(data, '\n') }
func digest(data []byte) string { h:=sha256.Sum256(data); return hex.EncodeToString(h[:]) }
func id() string { b:=make([]byte,8); _,err:=rand.Read(b); must(err); return hex.EncodeToString(b) }
func uuid7() string {
 b:=make([]byte,16); _,err:=rand.Read(b); must(err)
 n:=time.Now().UnixMilli(); for i:=5;i>=0;i-- { b[i]=byte(n); n>>=8 }
 b[6]=(b[6]&15)|0x70; b[8]=(b[8]&63)|0x80
 return fmt.Sprintf("%x-%x-%x-%x-%x",b[:4],b[4:6],b[6:8],b[8:10],b[10:])
}
func emit(v any) { _,err:=os.Stdout.Write(encode(v)); must(err) }

func main() {
 cwd,err:=os.Getwd(); must(err)
 if len(os.Args)==2 && os.Args[1]=="--hold-pipe" {
  time.Sleep(450*time.Millisecond)
  must(os.WriteFile(filepath.Join(cwd,"pipe-descendant-exited"),[]byte("done"),0600)); return
 }
 args:=map[string]string{}
 for i:=1;i<len(os.Args);i++ { switch os.Args[i] {
 case "--session", "--session-dir", "--model", "--thinking", "--tools", "--extension":
  if i+1>=len(os.Args) { panic("missing fixture argument") }; args[os.Args[i]]=os.Args[i+1]; i++
 case "--session-id", "--continue", "--resume", "--fork", "--no-session": panic("unexpected legacy/fallback option")
 } }
 path:=args["--session"]
 if !filepath.IsAbs(path) || args["--session-dir"]!=filepath.Dir(path) { panic("wrong exact fixture path") }
 raw,err:=os.ReadFile(path); must(err)
 info,err:=os.Stat(path); must(err)
 if info.Mode()!=0600 { panic("wrong fixture session mode") }
 config,err:=os.ReadFile(os.Getenv("AGENT_WORKSPACE_MEMBER_TOOL_CONFIG")); must(err)
 var credentials struct { Token string }; must(json.Unmarshal(config,&credentials))
 input,err:=io.ReadAll(os.Stdin); must(err); mode:=strings.TrimSpace(string(input))
 proof:=map[string]any{"pid":os.Getpid(),"argv":os.Args[1:],"token_sha":digest([]byte(credentials.Token)),"config_sha":digest(config),"input_sha":digest(input),"session_file":path,"fresh":len(raw)==0}
 must(os.WriteFile(filepath.Join(cwd,"fixture-proof.json"),encode(proof),0600))
 inv,err:=os.OpenFile(filepath.Join(cwd,"fixture-invocations"),os.O_WRONLY|os.O_CREATE|os.O_APPEND,0600); must(err)
 _,err=fmt.Fprintln(inv,os.Getpid()); must(err); must(inv.Close())
 var header map[string]any
 var parent any
 fresh:=len(raw)==0
 if fresh {
  header=map[string]any{"type":"session","version":3,"id":uuid7(),"cwd":cwd,"timestamp":time.Now().UTC().Format(time.RFC3339Nano)}
  must(os.WriteFile(path,encode(header),0600))
 } else {
  lines:=bytes.Split(bytes.TrimSpace(raw),[]byte{'\n'})
  must(json.Unmarshal(lines[0],&header))
  var last map[string]any; must(json.Unmarshal(lines[len(lines)-1],&last)); parent=last["id"]
 }
 if mode=="fixture:prefix-drift" {
  raw=bytes.Replace(raw,[]byte("fixture:fresh"),[]byte("fixture:FRESH"),1)
  must(os.WriteFile(path,raw,0600))
 }
 outputHeader:=map[string]any{}; for k,v:=range header { outputHeader[k]=v }
 if mode=="fixture:wrong-header" { outputHeader["cwd"]="/wrong-cwd" }
 if mode=="fixture:wrong-native-id" { outputHeader["id"]=uuid7() }
 emit(outputHeader)
 emit(map[string]any{"type":"agent_start"})
 emit(map[string]any{"type":"turn_start"})
 if mode=="fixture:cancel" { time.Sleep(30*time.Second); return }
 appendEntry:=func(entry map[string]any) {
  entry["id"]=id(); entry["parentId"]=parent; entry["timestamp"]=time.Now().UTC().Format(time.RFC3339Nano); parent=entry["id"]
  file,err:=os.OpenFile(path,os.O_WRONLY|os.O_APPEND,0600); must(err)
  _,err=file.Write(encode(entry)); must(err); must(file.Close())
 }
 persist:=func(message map[string]any) { appendEntry(map[string]any{"type":"message","message":message}) }
 // Pi 1.4 agent-loop.js emits a partial assistant start and an authoritative end.
 emitMessage:=func(body map[string]any) {
  start:=body
  if body["role"]=="assistant" {
   start=map[string]any{}; for k,v:=range body { start[k]=v }
   start["content"],start["stopReason"]=[]any{},"pending"
  }
  if body["role"]!="assistant" || mode!="fixture:missing-assistant-start" { emit(map[string]any{"type":"message_start","message":start}) }
  if body["role"]!="user" || mode!="fixture:missing-user-end" { emit(map[string]any{"type":"message_end","message":body}) }
 }
 message:=func(body map[string]any) {
  if body["timestamp"]==nil { body["timestamp"]=time.Now().UnixMilli() }
  emitMessage(body)
  persist(body)
 }
 model:=strings.SplitN(args["--model"],"/",2)
 tools:=strings.Split(args["--tools"],",")
 if fresh {
  appendEntry(map[string]any{"type":"model_change","provider":model[0],"modelId":model[1]})
  appendEntry(map[string]any{"type":"thinking_level_change","thinkingLevel":args["--thinking"]})
  declarations:=[]any{}; for _,name:=range tools { declarations=append(declarations,map[string]any{"name":name,"description":"offline fixture","parameters":map[string]any{}}) }
  message(map[string]any{"role":"system","content":"restricted offline text fixture","toolsAdded":declarations})
 }
 // Pi 1.4 agent-session.js constructs one text block for a plain stdin prompt.
 text:=mode; if mode=="fixture:wrong-user" { text="different prompt" }
 user:=map[string]any{"role":"user","content":[]any{map[string]any{"type":"text","text":text}}}
 if !fresh && (mode=="fixture:prior-response" || mode=="fixture:prior-response-after-user") {
  lines:=bytes.Split(bytes.TrimSpace(raw),[]byte{'\n'})
  for i:=len(lines)-1;i>0;i-- {
   var entry struct{ Message map[string]any }; must(json.Unmarshal(lines[i],&entry))
   if entry.Message["role"]!="assistant" { continue }
   if mode=="fixture:prior-response-after-user" {
    // A timestamp tie alone cannot disambiguate a known historical answer.
    user["timestamp"]=entry.Message["timestamp"]
    message(user)
   }
   emitMessage(entry.Message)
   emit(map[string]any{"type":"turn_end","message":entry.Message,"toolResults":[]any{}})
   emit(map[string]any{"type":"agent_end","messages":[]any{entry.Message},"willRetry":false})
   emit(map[string]any{"type":"agent_settled","aborted":false})
   return
  }
  panic("missing synthetic prior assistant")
 }
 if mode!="fixture:missing-user" { message(user) }
 if mode=="fixture:eof" { return }
 assistant:=func(content []any,stop string) map[string]any {
  return map[string]any{"role":"assistant","content":content,"provider":model[0],"model":model[1],"thinkingLevel":args["--thinking"],"stopReason":stop}
 }
 toolModes:=map[string]bool{"fixture:tool-ok":true,"fixture:unknown-write":true,"fixture:missing-tool-end":true,"fixture:mismatched-tool-id":true,"fixture:pending-tool":true,"fixture:write-error-file-clean":true,"fixture:mismatched-result":true,"fixture:foreign-tool":true}
 if toolModes[mode] {
  callID:=id(); name:="send_message"; if mode=="fixture:foreign-tool" { name="bash" }
  arguments:=map[string]any{"text":"offline only"}
  call:=assistant([]any{map[string]any{"type":"toolCall","id":callID,"name":name,"arguments":arguments}},"toolUse")
  message(call)
  startID:=callID; if mode=="fixture:mismatched-tool-id" { startID=id() }
  emit(map[string]any{"type":"tool_execution_start","toolCallId":startID,"toolName":name,"args":arguments})
  if mode!="fixture:pending-tool" {
   content:=[]any{map[string]any{"type":"text","text":"offline result"}}
   isError:=mode=="fixture:unknown-write"
   endContent:=content; if mode=="fixture:mismatched-result" { endContent=[]any{map[string]any{"type":"text","text":"different result"}} }
   if mode!="fixture:missing-tool-end" { emit(map[string]any{"type":"tool_execution_end","toolCallId":callID,"toolName":name,"result":map[string]any{"content":endContent},"isError":isError||mode=="fixture:write-error-file-clean"}) }
   result:=map[string]any{"role":"toolResult","toolCallId":callID,"toolName":name,"content":content,"isError":isError}
   message(result)
   emit(map[string]any{"type":"turn_end","message":call,"toolResults":[]any{result}})
   emit(map[string]any{"type":"turn_start"})
  }
 }
 final:=assistant([]any{map[string]any{"type":"text","text":"offline complete"}},"stop")
 if mode=="fixture:stale-assistant" { final["timestamp"]=1 }
 if mode=="fixture:invalid-assistant-timestamp" { final["timestamp"]=json.Number("9223372036854775808") }
 if mode=="fixture:wrong-model" { final["model"]="other" }
 if mode=="fixture:wrong-thinking" { final["thinkingLevel"]="off" }
 if mode=="fixture:after-settled" {
  final["timestamp"]=time.Now().UnixMilli()
  emitMessage(final)
 } else { message(final) }
 emit(map[string]any{"type":"turn_end","message":final,"toolResults":[]any{}})
 emit(map[string]any{"type":"agent_end","messages":[]any{},"willRetry":false})
 if mode=="fixture:queue" { emit(map[string]any{"type":"queue_update","steering":[]any{},"followUp":[]any{"unexpected"}}) }
 emit(map[string]any{"type":"agent_settled","aborted":false})
 if mode=="fixture:after-settled" { time.Sleep(150*time.Millisecond); persist(final) }
 appendEntry(map[string]any{"type":"usage","kind":"cache_warm","provider":model[0],"model":model[1],"usage":map[string]any{}})
 if mode=="fixture:bad-lf" { stat,err:=os.Stat(path); must(err); must(os.Truncate(path,stat.Size()-1)) }
 if mode=="fixture:replace-file" { data,err:=os.ReadFile(path); must(err); must(os.WriteFile(path+".new",data,0600)); must(os.Rename(path+".new",path)) }
 if mode=="fixture:output-tail" { _,err:=os.Stdout.Write([]byte("{partial")); must(err) }
 if mode=="fixture:inherited-pipe" {
  child:=exec.Command(os.Args[0],"--hold-pipe"); child.Stdout=os.Stdout; child.Stderr=os.Stderr; must(child.Start())
 }
 must(os.WriteFile(filepath.Join(cwd,"fixture-finished"),[]byte("finished"),0600))
}
`
