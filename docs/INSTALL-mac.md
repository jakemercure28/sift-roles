# Installing on a Mac (no tech experience needed)

This takes about 10 to 15 minutes. Most of it is the computer downloading
things while you wait. You will paste in one line, click a few buttons, and
type your Mac password once. That is it.

## What you do

1. **Open the Terminal app.**
   Hold the **Command** key and press the **Space bar**. A search box appears in
   the top corner. Type the word **Terminal** and press **Return**. A plain text
   window opens. This is Terminal.

2. **Paste in this one line and press Return.**

   ```
   /bin/bash -c "$(curl -fsSL https://raw.githubusercontent.com/jakemercure28/job-search-automation/main/install.sh)"
   ```

   To paste: click once in the Terminal window, then hold **Command** and press
   **V**. Then press **Return**. It starts working. Leave the window open and let
   it run.

3. **Click through the prompts as they appear.** You do not need to understand
   them. Here is everything you might see:
   - A window titled **"Command Line Developer Tools."** Click **Install** and
     wait for it to finish.
   - A line asking for your **password.** This is your normal Mac login
     password. Type it and press Return. You will not see the letters appear as
     you type. That is normal.
   - A window for **OrbStack** (the program that runs the app). If it asks for
     anything, click to accept or allow.

4. **Wait for it to finish.**
   When everything is ready, your web browser opens by itself to the dashboard.
   The text window will say **"All set."** You can close that window.

## What you see when it works

Your dashboard opens at **http://localhost:3131**. A short setup wizard on that
page walks you through the last two things:

- A **free Gemini key** from Google. Get it here, no credit card needed:
  https://aistudio.google.com/apikey
- Your **resume** and what kind of jobs you want.

## If something goes wrong

- **The window stopped with a message instead of finishing.** Paste the same
  line in again and press Return. It picks up where it left off and skips
  anything already done. It is safe to run as many times as you need.
- **The browser did not open on its own.** Open your browser and type
  `http://localhost:3131` into the address bar.
- **Still stuck.** Take a photo or screenshot of the text window and send it
  over so it can be sorted out.
