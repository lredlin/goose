From RecoveryRefinement.Goose Require Import base.

Module partialFile.
  Record t {model:GoModel} := mk {
    off: uint64;
    data: slice.t byte;
  }.
  Arguments mk {model}.
  Global Instance t_zero {model:GoModel} : HasGoZero t := mk (zeroValue _) (zeroValue _).
End partialFile.

Definition readMessage {model:GoModel} (name:string) : proc (slice.t byte) :=
  f <- FS.open name;
  fileContents <- Data.newPtr (slice.t byte);
  _ <- Loop (fun pf =>
        buf <- FS.readAt f pf.(partialFile.off) 4096;
        newData <- Data.sliceAppendSlice pf.(partialFile.data) buf;
        if compare_to (slice.length buf) 4096 Lt
        then
          _ <- Data.writePtr fileContents newData;
          LoopRet tt
        else
          Continue {| partialFile.off := pf.(partialFile.off);
                      partialFile.data := newData; |}) {| partialFile.off := 0;
           partialFile.data := slice.nil _; |};
  fileData <- Data.readPtr fileContents;
  Ret fileData.

(* Pickup reads all stored messages *)
Definition Pickup {model:GoModel} : proc (slice.t (slice.t byte)) :=
  names <- FS.list;
  messages <- Data.newPtr (slice.t (slice.t byte));
  initMessages <- Data.newSlice (slice.t byte) 0;
  _ <- Data.writePtr messages initMessages;
  _ <- Loop (fun i =>
        if i == slice.length names
        then LoopRet tt
        else
          name <- Data.sliceRead names i;
          msg <- readMessage name;
          oldMessages <- Data.readPtr messages;
          newMessages <- Data.sliceAppend oldMessages msg;
          _ <- Data.writePtr messages newMessages;
          Continue (i + 1)) 0;
  msgs <- Data.readPtr messages;
  Ret msgs.

Definition writeAll {model:GoModel} (fname:string) (data:slice.t byte) : proc unit :=
  f <- FS.create fname;
  _ <- Loop (fun buf =>
        if compare_to (slice.length buf) 4096 Lt
        then
          _ <- FS.append f buf;
          LoopRet tt
        else
          _ <- FS.append f (slice.take 4096 buf);
          Continue (slice.skip 4096 buf)) data;
  FS.close f.

(* Deliver stores a new message

   tid should be a unique thread ID (used as a helper for spooling the message). *)
Definition Deliver {model:GoModel} (tid:string) (msg:slice.t byte) : proc unit :=
  _ <- writeAll tid msg;
  initID <- Data.randomUint64;
  Loop (fun id =>
        ok <- FS.link tid ("msg" ++ uint64_to_string id);
        if ok
        then LoopRet tt
        else
          newID <- Data.randomUint64;
          Continue newID) initID.
