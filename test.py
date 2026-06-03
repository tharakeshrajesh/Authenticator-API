import sqlite3
import hashlib

# create user

api_key = "12345678901234567890123456789012"

conn = sqlite3.connect(".db")
conn.execute("""
    INSERT INTO users (api_key, email, username, password, reqsperday)
    VALUES (?, ?, ?, ?, ?)
""", (api_key, "test@test.com", "testuser", hashlib.sha256("Test123!".encode()).hexdigest(), 15))
conn.commit()
conn.close()

print("api key:", api_key)

# print all tables

# conn = sqlite3.connect(".db")
# cursor = conn.cursor()

# cursor.execute("SELECT name FROM sqlite_master WHERE type='table';")
# tables = cursor.fetchall()

# for (table_name,) in tables:
#     print(f"\n=== TABLE: {table_name} ===")

#     cursor.execute(f"SELECT * FROM {table_name}")
#     rows = cursor.fetchall()

#     for row in rows:
#         print(row)
#         print("")

# conn.close()