# Setup walkthrough

Run the walkthrough from this folder:

```bash
python setup.py
```

If Python reports that it is missing, install a current Python release, reopen your terminal, and run the command again. The walkthrough creates `.venv` and installs the dependencies. After that step, you should see the next question instead of a Python error.

## Answer the questions

The setup asks why you want a knowledge base first. It saves that answer in `config.json` so a later session can understand what the library is for.

The starter stores chunks and vectors in `kb.sqlite` on your computer. The local file keeps setup simple and lets the hook retrieve the text it needs without another service.

Choose **free** unless you want to use a paid embedding API. Free mode installs a local embedding model. The first model download needs internet access. Paid mode asks for an API key without displaying it, calculates the expected embedding cost, and waits for you to type `yes`.

Select only the sources you want now. You can rerun `python setup.py` later to add another source or change the schedule.

Choose **sample** before a large backfill. The walkthrough ingests a small selection, then you run the retrieval evaluation. If the report returns the wrong material or answers an unrelated question, fix that before importing the rest of the library.

If you choose a daily refresh, setup asks for a time. If you choose weekly, it also asks for a day. Setup installs a LaunchAgent on macOS or a Task Scheduler task on Windows. You should see the created configuration path after setup. If the computer is asleep at the scheduled time, that refresh waits until you run it manually:

```bash
python ingest.py
```

## Test recall

Start a new Claude Code or Codex session after setup writes the hook. Ask a question that appears in a selected source. You should see a `KB RECALL` block containing material from that source. If you do not, run this command from the package folder:

```bash
python kb.py stats
```

You should see a nonzero count for the source you selected. If the count is zero, run `python ingest.py` and read the source error printed in the terminal.

After you have used the hook for a few prompts, review what it put into those prompts:

```bash
python chunk_review.py --hours 24
```

Mark the chunks that helped as Useful and the others as Junk, then click Save. After at least three reviewed prompts and eight reviewed chunks, the review tests the actual prompts again at three, four, and six chunks. It writes the smallest limit that keeps the useful chunks into `config.json`.
