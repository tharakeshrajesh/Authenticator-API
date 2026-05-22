import requests, sys
from os import getenv
from dotenv import load_dotenv
from cryptography.fernet import Fernet
from json import loads

load_dotenv()

key = getenv("FERNET_KEY")

def decrypt(text):
    f = Fernet(key)
    return f.decrypt(text.encode()).decode()

number = sys.argv[1]
code = sys.argv[2]

session = requests.Session()

thing1 = session.post(decrypt("gAAAAABqENQndLkWjTtwy9Z7KbNFgPoJo3RJKyhENLl70xIH9tVhfvjABQQHYPmEIzR_1UlgqyXiopGfVrp_3j3Vzn6LwXwexOuFf-9h4MG07r6A__ndVvI_gy02PXbK6JBxw-M938wY"), data={
    "email": getenv("SMTP_EMAIL"),
    "password": getenv("TEXT_PASS"),
    decrypt("gAAAAABqENSoJ2jagmwlgpHcr-R_LJB6w_hHkQljCiwiv3YPGhTUB4OshQKziKx6mGYPTaBLHsulh1BEiCKKjJyaNwNkpIvSQg=="): "9d4e8b1fa7632c5d0ab4478efc91d2a6",
    "utm_params": "",
})

thing2 = session.post(decrypt("gAAAAABqENT5N5uSCZseLBuX1HkHmePjFcAvNc10cJ5hjRaiiPLKy93hkDly5Qgmh3AW3NZJSWg-ljCF2XXVE0PEectqtyTOtlHy5TUcN_TIIVMIlgjDjt0CY3V5FYw5yYWjFjgjkPEEcdLaxfaG_e26vKrY67qhUzJwODgWLJ0PaVxm0Bj316Y="), data={
    decrypt("gAAAAABqENTDAFzrO_u2Sxy4IPEANuDPewObO8aMUvRgdRdZN04nss9cE-Q4Z1BrzhIuqzbnuTbM4X86ZoC3yfX0FV7TS-qdrw=="): decrypt("gAAAAABqENTk6lYYY1MWaunNmrpsbmk-6buSuOAcTik2Q137Bsyycq-kzGSVWykNOHje0gO99EyYSuZ8ctpwEDvfDL-OxqK1v_M0WlqnmfQSLZcO2PIs4DZ2ubU0WZw1xUfGKmygAWZW"),
    decrypt("gAAAAABqENVRSIUOQsh1xfcdTRsnVV2t-Qam1s6o3xxTJxqJeSW9ZhVUqQjt1LHhBiDbPNlFpjsWGhRnsSj0nrmvvbYdBQx-KQ=="): "834443883236ad494b076dffee8c76b8",
}, headers={
    "X-Requested-With": "XMLHttpRequest",
})

thing3 = session.post(decrypt("gAAAAABqENUanbYHao6-pddcPivCgg3fNY15DjvzPNQrbmA5u544sz8yutrb0Nk3oX5pQRprup9HK0fi8XvBeTsOEs49pP89ID-nvtESUB2eDQFBnvnid9Y2hIWVEQugAXyI_sn95Z_j"), data={
    "phone_number": number,
    "message_content": f"Your verification code: {code}.",
    decrypt("gAAAAABqENaa9v4mVBDvUFhKB-SMKG2sIBWl88Ci9HLMcwaAQM-7aObhR3Ki492Nv4z04WP9qcPfiPMDWQjQ7TICyj8NhbSj9Q=="): decrypt("gAAAAABqENaaETiTweSbnl044Uac-MhDdJky66WHGGdV-GFqmjN9e1QsosI7K7KbvbSB3_FKj9QlfWW9WUspIBwyIf96P7o1OA=="),
    decrypt("gAAAAABqENU6O0OphK_I-t5FawqpYCH1U049FCimsBQxdZsK5J6FwTRGCn8BK_ituVervuLN-_ItzKWy6fdcIwN3mTzgBa3wLg=="): "f1a92c77b54e3d8a4c0b9e6d2f71ab35",
})
print(loads(thing3.text)["result"])

# sorry for reverse engineering this website but it was my last option
# i cant show which site or how so i have to encrypt, sorry guys
